package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// JDownloader talks to JDownloader 2's local HTTP API ("Deprecated API",
// advanced setting RemoteAPI: DeprecatedApiEnabled, port 3128). It is the
// MyJDownloader API without the cloud: /<namespace>/<method>?<params>, each
// parameter a URL-encoded JSON value. No account and no connection to
// my.jdownloader.org; it has no authentication, so only drop may reach it
// (NetworkPolicy in the cluster).
//
// Links are added to the link grabber, not straight to the download list:
// JDownloader crawls a page it has no plugin for with its generic parser,
// which finds every image and thumbnail on it. Either the owner picks the
// files in drop's review list, or (review off) drop starts the relevant ones
// itself once the crawl is done (pickRelevant) and removes the rest.
type JDownloader struct {
	url  string
	http *http.Client

	// crawlTimeout bounds how long an automatic start waits for a crawl;
	// pollEvery is how often it checks.
	crawlTimeout, pollEvery time.Duration

	mu       sync.Mutex
	autoJobs map[int64]bool // crawl jobs drop starts by itself
}

func NewJDownloader(baseURL string) *JDownloader {
	return &JDownloader{url: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: httpTimeout},
		crawlTimeout: 10 * time.Minute, pollEvery: 2 * time.Second, autoJobs: map[int64]bool{}}
}

// call invokes namespace/method with positional params and decodes the
// result (bare, or wrapped in {"data": ...} as the MyJDownloader API does)
// into out.
func (j *JDownloader) call(ctx context.Context, endpoint string, out any, params ...any) error {
	var q []string
	for _, p := range params {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		q = append(q, url.QueryEscape(string(b)))
	}
	u := j.url + "/" + endpoint
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return fmt.Errorf("jdownloader: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jdownloader: HTTP %d: %s", resp.StatusCode, firstLine(body))
	}
	if out == nil {
		return nil
	}
	var wrapped struct {
		Data json.RawMessage `json:"data"`
	}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '{' && json.Unmarshal(t, &wrapped) == nil && wrapped.Data != nil {
		body = wrapped.Data
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("jdownloader: %w", err)
	}
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (j *JDownloader) Add(ctx context.Context, it Item, _ string) error {
	var job struct {
		ID int64 `json:"id"`
	}
	switch it.Kind {
	case KindLink, KindTorrentURL, KindNZBURL:
		if err := j.call(ctx, "linkgrabberv2/addLinks", &job, map[string]any{"links": it.URL, "autostart": false}); err != nil {
			return err
		}
	case KindDLCFile:
		data := "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(it.Data)
		if err := j.call(ctx, "linkgrabberv2/addContainer", &job, "DLC", data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("JDownloader can't take a %s", it.Kind)
	}
	if !it.Review && job.ID != 0 {
		j.mu.Lock()
		j.autoJobs[job.ID] = true
		j.mu.Unlock()
		go j.autoStart(job.ID)
	}
	return nil
}

// JDownloader has no categories: everything goes to its download folder.
func (j *JDownloader) Categories(context.Context) ([]string, error) { return nil, nil }

// finishedShown is how many finished packages the status view keeps
// (JDownloader keeps them in its list until they're removed by hand, without
// a completion time).
const finishedShown = 10

type jdPackage struct {
	UUID        json.Number `json:"uuid"`
	Name        string      `json:"name"`
	BytesLoaded int64       `json:"bytesLoaded"`
	BytesTotal  int64       `json:"bytesTotal"`
	Speed       int64       `json:"speed"`
	ETA         int64       `json:"eta"`
	Finished    bool        `json:"finished"`
	Running     bool        `json:"running"`
	Enabled     bool        `json:"enabled"`
	Status      string      `json:"status"`
}

func (j *JDownloader) Downloads(ctx context.Context) ([]Download, error) {
	var pkgs []jdPackage
	query := map[string]any{"bytesLoaded": true, "bytesTotal": true, "speed": true, "eta": true, "finished": true,
		"running": true, "enabled": true, "status": true, "maxResults": -1, "startAt": 0}
	if err := j.call(ctx, "downloadsV2/queryPackages", &pkgs, query); err != nil {
		return nil, err
	}
	var active, finished []Download
	for _, p := range pkgs {
		d := Download{Source: TargetJDownloader, ID: p.UUID.String(), Name: p.Name, Size: p.BytesTotal, Done: p.BytesLoaded,
			Speed: p.Speed, ETA: p.ETA, Detail: p.Status}
		if d.ETA <= 0 {
			d.ETA = -1
		}
		if p.BytesTotal > 0 {
			d.Progress = float64(p.BytesLoaded) / float64(p.BytesTotal)
		}
		switch {
		case p.Finished:
			d.State, d.Progress = "done", 1
			finished = append(finished, d)
			continue
		case p.Running:
			d.State = "downloading"
		case !p.Enabled:
			d.State = "paused"
		default:
			d.State = "queued"
		}
		active = append(active, d)
	}
	if len(finished) > finishedShown {
		finished = finished[len(finished)-finishedShown:]
	}

	return append(active, finished...), nil
}

// --- Link grabber review ----------------------------------------------------

// ReviewLink is one file JDownloader found for a package.
type ReviewLink struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	Availability string `json:"availability"` // ONLINE, OFFLINE, UNKNOWN, TEMP_UNKNOWN
	URL          string `json:"url,omitempty"`
	Selected     bool   `json:"selected"` // pickRelevant's suggestion
}

// ReviewPackage is one package in JDownloader's link grabber.
type ReviewPackage struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Auto  bool         `json:"auto"` // drop starts it by itself once crawled
	Links []ReviewLink `json:"links"`
}

// Review lists the link grabber: packages with their files and the
// suggested selection; crawling is true while JDownloader is still
// collecting or checking links.
func (j *JDownloader) Review(ctx context.Context) ([]ReviewPackage, bool, error) {
	var collecting bool
	if err := j.call(ctx, "linkgrabberv2/isCollecting", &collecting); err != nil {
		return nil, false, err
	}
	var pkgs []struct {
		UUID json.Number `json:"uuid"`
		Name string      `json:"name"`
	}
	if err := j.call(ctx, "linkgrabberv2/queryPackages", &pkgs, map[string]any{"maxResults": -1, "startAt": 0}); err != nil {
		return nil, false, err
	}
	links, err := j.links(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	auto := j.autoPackages(ctx)
	out := make([]ReviewPackage, 0, len(pkgs))
	for _, p := range pkgs {
		rp := ReviewPackage{ID: p.UUID.String(), Name: p.Name, Auto: auto[p.UUID.String()], Links: []ReviewLink{}}
		for _, l := range links {
			if l.PackageUUID.String() == rp.ID {
				rp.Links = append(rp.Links, ReviewLink{ID: l.UUID.String(), Name: l.Name, Size: l.BytesTotal,
					Availability: l.Availability, URL: l.URL})
			}
		}
		pick := pickRelevant(rp.Links)
		for i := range rp.Links {
			rp.Links[i].Selected = pick[i]
		}
		out = append(out, rp)
	}
	return out, collecting, nil
}

type jdLink struct {
	UUID         json.Number `json:"uuid"`
	PackageUUID  json.Number `json:"packageUUID"`
	Name         string      `json:"name"`
	BytesTotal   int64       `json:"bytesTotal"`
	Availability string      `json:"availability"`
	URL          string      `json:"url"`
}

// links lists the link grabber's links, of the given crawl jobs or all.
func (j *JDownloader) links(ctx context.Context, jobs []int64) ([]jdLink, error) {
	q := map[string]any{"bytesTotal": true, "availability": true, "url": true, "maxResults": -1, "startAt": 0}
	if jobs != nil {
		q["jobUUIDs"] = jobs
	}
	var links []jdLink
	err := j.call(ctx, "linkgrabberv2/queryLinks", &links, q)
	return links, err
}

// autoPackages are the link grabber packages of crawl jobs drop starts by
// itself (so the review list can show them as such).
func (j *JDownloader) autoPackages(ctx context.Context) map[string]bool {
	j.mu.Lock()
	var jobs []int64
	for id := range j.autoJobs {
		jobs = append(jobs, id)
	}
	j.mu.Unlock()
	out := map[string]bool{}
	if len(jobs) == 0 {
		return out
	}
	links, err := j.links(ctx, jobs)
	if err != nil {
		return out
	}
	for _, l := range links {
		out[l.PackageUUID.String()] = true
	}
	return out
}

// Start moves the selected links to the download list and removes the
// other links of their packages from the link grabber.
func (j *JDownloader) Start(ctx context.Context, start, remove []int64) error {
	if len(start) > 0 {
		if err := j.call(ctx, "linkgrabberv2/moveToDownloadlist", nil, start, []int64{}); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		return j.call(ctx, "linkgrabberv2/removeLinks", nil, remove, []int64{})
	}
	return nil
}

// Discard removes whole packages from the link grabber.
func (j *JDownloader) Discard(ctx context.Context, packages []int64) error {
	return j.call(ctx, "linkgrabberv2/removeLinks", nil, []int64{}, packages)
}

// autoStart waits until crawl job id is done, then starts the relevant
// links of its packages and removes the rest.
func (j *JDownloader) autoStart(id int64) {
	defer func() {
		j.mu.Lock()
		delete(j.autoJobs, id)
		j.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), j.crawlTimeout)
	defer cancel()
	for {
		var jobs []struct {
			JobID    json.Number `json:"jobId"`
			Crawling bool        `json:"crawling"`
			Checking bool        `json:"checking"`
		}
		err := j.call(ctx, "linkgrabberv2/queryLinkCrawlerJobs", &jobs, map[string]any{"jobIds": []int64{id}, "collectorInfo": true})
		if err != nil {
			log.Printf("jdownloader auto start %d: %v", id, err)
			return
		}
		busy := false
		for _, jb := range jobs {
			busy = busy || jb.Crawling || jb.Checking
		}
		if !busy {
			break
		}
		select {
		case <-ctx.Done():
			log.Printf("jdownloader auto start %d: crawl not done after %s, left in the link grabber", id, j.crawlTimeout)
			return
		case <-time.After(j.pollEvery):
		}
	}
	links, err := j.links(ctx, []int64{id})
	if err != nil {
		log.Printf("jdownloader auto start %d: %v", id, err)
		return
	}
	byPkg := map[string][]int{}
	var order []string
	for i, l := range links {
		p := l.PackageUUID.String()
		if _, ok := byPkg[p]; !ok {
			order = append(order, p)
		}
		byPkg[p] = append(byPkg[p], i)
	}
	var start, remove []int64
	for _, p := range order {
		var rl []ReviewLink
		for _, i := range byPkg[p] {
			rl = append(rl, ReviewLink{Name: links[i].Name, Size: links[i].BytesTotal, Availability: links[i].Availability})
		}
		pick := pickRelevant(rl)
		for k, i := range byPkg[p] {
			n, _ := links[i].UUID.Int64()
			if pick[k] {
				start = append(start, n)
			} else {
				remove = append(remove, n)
			}
		}
	}
	if err := j.Start(ctx, start, remove); err != nil {
		log.Printf("jdownloader auto start %d: %v", id, err)
		return
	}
	log.Printf("jdownloader auto start %d: started %d, removed %d", id, len(start), len(remove))
}

// File types for pickRelevant.
var (
	imageExt = setOf("jpg", "jpeg", "png", "gif", "webp", "bmp", "avif", "heic", "svg", "ico", "tif", "tiff")
	junkExt  = setOf("html", "htm", "php", "asp", "aspx", "js", "css", "json", "xml", "txt", "nfo", "url", "sfv", "md5", "srt")
)

func setOf(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func ext(name string) string { return strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".") }

// pickRelevant suggests which links of one package to download: offline
// links never; if the package has anything besides images and page junk
// (videos, archives, audio, documents, unknown files), only those; if it is
// nothing but images (an album), the images.
func pickRelevant(links []ReviewLink) []bool {
	pick := make([]bool, len(links))
	hasMain := false
	for _, l := range links {
		e := ext(l.Name)
		if l.Availability != "OFFLINE" && !imageExt[e] && !junkExt[e] {
			hasMain = true
		}
	}
	for i, l := range links {
		if l.Availability == "OFFLINE" {
			continue
		}
		e := ext(l.Name)
		if hasMain {
			pick[i] = !imageExt[e] && !junkExt[e]
		} else {
			pick[i] = imageExt[e]
		}
	}
	return pick
}
