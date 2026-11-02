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

	// Automatic starts run one at a time (autoMu): the API doesn't tell
	// which crawled links belong to which addLinks call (no job ids on the
	// links, no crawler jobs listed), so each one takes the links that are
	// new compared to a snapshot taken before its add. pending holds the
	// snapshots of starts still waiting, for the review list.
	autoMu  sync.Mutex
	mu      sync.Mutex
	pending []*snapshot
}

// snapshot is the link grabber's link ids before an automatic start's add.
type snapshot struct{ ids map[string]bool }

func NewJDownloader(baseURL string) *JDownloader {
	return &JDownloader{url: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: httpTimeout},
		crawlTimeout: 10 * time.Minute, pollEvery: 2 * time.Second}
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
	if !it.Review {
		// Hold the lock from the snapshot to the end of the crawl, so two
		// automatic starts don't take each other's links.
		j.autoMu.Lock()
		before, err := j.linkIDs(ctx)
		if err != nil {
			j.autoMu.Unlock()
			return err
		}
		if err := j.add(ctx, it); err != nil {
			j.autoMu.Unlock()
			return err
		}
		snap := &snapshot{before}
		j.mu.Lock()
		j.pending = append(j.pending, snap)
		j.mu.Unlock()
		go func() {
			defer j.autoMu.Unlock()
			j.autoStart(snap)
		}()
		return nil
	}
	return j.add(ctx, it)
}

// add puts an item into the link grabber, without autostart.
func (j *JDownloader) add(ctx context.Context, it Item) error {
	switch it.Kind {
	case KindLink, KindTorrentURL, KindNZBURL:
		return j.call(ctx, "linkgrabberv2/addLinks", nil, map[string]any{"links": it.URL, "autostart": false})
	case KindDLCFile:
		data := "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(it.Data)
		return j.call(ctx, "linkgrabberv2/addContainer", nil, "DLC", data)
	}
	return fmt.Errorf("JDownloader can't take a %s", it.Kind)
}

// linkIDs is the set of links currently in the link grabber.
func (j *JDownloader) linkIDs(ctx context.Context) (map[string]bool, error) {
	links, err := j.links(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, l := range links {
		ids[l.UUID.String()] = true
	}
	return ids, nil
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
	links, err := j.links(ctx)
	if err != nil {
		return nil, false, err
	}
	auto := j.autoPackages(links)
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

// links lists the link grabber's links.
func (j *JDownloader) links(ctx context.Context) ([]jdLink, error) {
	q := map[string]any{"bytesTotal": true, "availability": true, "url": true, "maxResults": -1, "startAt": 0}
	var links []jdLink
	err := j.call(ctx, "linkgrabberv2/queryLinks", &links, q)
	return links, err
}

// autoPackages are the packages with links that are new compared to a
// waiting automatic start's snapshot (so the review list can show them as
// starting by themselves).
func (j *JDownloader) autoPackages(links []jdLink) map[string]bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]bool{}
	for _, snap := range j.pending {
		for _, l := range links {
			if !snap.ids[l.UUID.String()] {
				out[l.PackageUUID.String()] = true
			}
		}
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

// autoStart waits until JDownloader has finished collecting the links
// added after the snapshot (not collecting, and the link list unchanged
// between two checks), then starts the relevant new links per package and
// removes the other new ones.
func (j *JDownloader) autoStart(snap *snapshot) {
	before := snap.ids
	defer func() {
		j.mu.Lock()
		for i, p := range j.pending {
			if p == snap {
				j.pending = append(j.pending[:i], j.pending[i+1:]...)
				break
			}
		}
		j.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), j.crawlTimeout)
	defer cancel()

	var links []jdLink
	last := -1
	for {
		select {
		case <-ctx.Done():
			log.Printf("jdownloader auto start: links not settled after %s, left in the link grabber", j.crawlTimeout)
			return
		case <-time.After(j.pollEvery):
		}
		var collecting bool
		if err := j.call(ctx, "linkgrabberv2/isCollecting", &collecting); err != nil {
			log.Printf("jdownloader auto start: %v", err)
			return
		}
		var err error
		if links, err = j.links(ctx); err != nil {
			log.Printf("jdownloader auto start: %v", err)
			return
		}
		checking := false
		for _, l := range links {
			if !before[l.UUID.String()] && (l.Availability == "" || l.Availability == "UNKNOWN") {
				checking = true // not checked yet: no name/size to judge
			}
		}
		if !collecting && !checking && len(links) == last {
			break
		}
		last = len(links)
	}

	byPkg := map[string][]jdLink{}
	var order []string
	for _, l := range links {
		if before[l.UUID.String()] {
			continue
		}
		p := l.PackageUUID.String()
		if _, ok := byPkg[p]; !ok {
			order = append(order, p)
		}
		byPkg[p] = append(byPkg[p], l)
	}
	var start, remove []int64
	for _, p := range order {
		var rl []ReviewLink
		for _, l := range byPkg[p] {
			rl = append(rl, ReviewLink{Name: l.Name, Size: l.BytesTotal, Availability: l.Availability})
		}
		pick := pickRelevant(rl)
		for k, l := range byPkg[p] {
			n, _ := l.UUID.Int64()
			if pick[k] {
				start = append(start, n)
			} else {
				remove = append(remove, n)
			}
		}
	}
	if err := j.Start(ctx, start, remove); err != nil {
		log.Printf("jdownloader auto start: %v", err)
		return
	}
	log.Printf("jdownloader auto start: started %d, removed %d", len(start), len(remove))
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
