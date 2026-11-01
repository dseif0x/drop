package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// JDownloader talks to JDownloader 2's local HTTP API ("Deprecated API",
// advanced setting RemoteAPI: DeprecatedApiEnabled, port 3128). It is the
// MyJDownloader API without the cloud: /<namespace>/<method>?<params>, each
// parameter a URL-encoded JSON value. No account and no connection to
// my.jdownloader.org; it has no authentication, so only drop may reach it
// (NetworkPolicy in the cluster).
type JDownloader struct {
	url  string
	http *http.Client
}

func NewJDownloader(baseURL string) *JDownloader {
	return &JDownloader{url: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: httpTimeout}}
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
	switch it.Kind {
	case KindLink, KindTorrentURL, KindNZBURL:
		// autostart: crawled links go from the link grabber straight to the
		// download list, into JDownloader's default download folder.
		return j.call(ctx, "linkgrabberv2/addLinks", nil, map[string]any{"links": it.URL, "autostart": true})
	case KindDLCFile:
		data := "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(it.Data)
		return j.call(ctx, "linkgrabberv2/addContainer", nil, "DLC", data)
	}
	return fmt.Errorf("JDownloader can't take a %s", it.Kind)
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

	// Links still in the link grabber: being crawled, or waiting because
	// they couldn't be checked (offline, captcha, unsupported).
	var grabber []jdPackage
	if err := j.call(ctx, "linkgrabberv2/queryPackages", &grabber,
		map[string]any{"bytesTotal": true, "status": true, "maxResults": -1, "startAt": 0}); err != nil {
		return nil, err
	}
	for _, p := range grabber {
		active = append(active, Download{Source: TargetJDownloader, ID: "lg-" + p.UUID.String(), Name: p.Name,
			State: "crawling", Detail: orDefault(p.Status, "in the link grabber"), Size: p.BytesTotal, ETA: -1})
	}
	return append(active, finished...), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
