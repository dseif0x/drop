package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeluge(t *testing.T) {
	var calls []string
	loggedIn, connected := false, false
	labels := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
			ID     int    `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Method)
		reply := func(result any) {
			json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil, "id": req.ID})
		}
		if req.Method == "auth.login" {
			if req.Params[0] == "pw" {
				http.SetCookie(w, &http.Cookie{Name: "_session_id", Value: "s"})
				reply(true)
			} else {
				reply(false)
			}
			return
		}
		if c, err := r.Cookie("_session_id"); err != nil || c.Value != "s" {
			json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"message": "Not authenticated", "code": 1}, "id": req.ID})
			return
		}
		loggedIn = true
		switch req.Method {
		case "web.connected":
			reply(connected)
		case "web.get_hosts":
			reply([][]any{{"hostid1", "127.0.0.1", 58846, "Offline"}})
		case "web.connect":
			if req.Params[0] != "hostid1" {
				t.Errorf("connect %v", req.Params)
			}
			connected = true
			reply(nil)
		case "core.add_torrent_magnet":
			reply("abc123")
		case "core.add_torrent_file":
			if req.Params[0] != "x.torrent" || req.Params[1] != "ZGF0YQ==" {
				t.Errorf("file params %v", req.Params)
			}
			reply("def456")
		case "label.set_torrent":
			labels[req.Params[0].(string)] = req.Params[1].(string)
			reply(nil)
		case "label.get_labels":
			reply([]string{"tv", "movies"})
		case "core.get_torrents_status":
			reply(map[string]any{
				"abc123": map[string]any{"name": "A", "state": "Downloading", "progress": 50.0, "total_wanted": 100, "total_done": 50,
					"download_payload_rate": 10, "eta": 5, "label": "tv", "time_added": 2},
				"old": map[string]any{"name": "B", "state": "Paused", "progress": 100.0, "total_wanted": 10, "total_done": 10, "eta": 0, "time_added": 1},
			})
		default:
			t.Errorf("unexpected %s", req.Method)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	d := NewDeluge(srv.URL, "pw")
	if err := d.Add(ctx, Item{Kind: KindMagnet, URL: "magnet:?xt=1"}, "tv"); err != nil {
		t.Fatal(err)
	}
	// First call fails unauthenticated, then login, connect, retry, label.
	if want := "core.add_torrent_magnet auth.login web.connected web.get_hosts web.connect core.add_torrent_magnet label.set_torrent"; strings.Join(calls, " ") != want {
		t.Fatalf("calls %q", calls)
	}
	if !loggedIn || labels["abc123"] != "tv" {
		t.Fatal("label not set")
	}
	if err := d.Add(ctx, Item{Kind: KindTorrentFile, Filename: "x.torrent", Data: []byte("data")}, ""); err != nil {
		t.Fatal(err)
	}
	cats, _ := d.Categories(ctx)
	if strings.Join(cats, ",") != "movies,tv" {
		t.Fatalf("cats %v", cats)
	}
	dl, err := d.Downloads(ctx)
	if err != nil || len(dl) != 2 || dl[0].ID != "abc123" || dl[0].Progress != 0.5 || dl[0].State != "downloading" || dl[1].State != "done" || dl[1].ETA != -1 {
		t.Fatalf("downloads %+v %v", dl, err)
	}
	if err := NewDeluge(srv.URL, "wrong").Add(ctx, Item{Kind: KindMagnet, URL: "magnet:?x"}, ""); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestSABnzbd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("apikey") != "key" {
			io.WriteString(w, `{"status": false, "error": "API Key Incorrect"}`)
			return
		}
		switch q.Get("mode") {
		case "addurl":
			if q.Get("name") != "https://n.example/x.nzb" || q.Get("cat") != "movies" {
				t.Errorf("addurl %v", q)
			}
			io.WriteString(w, `{"status": true, "nzo_ids": ["SABnzbd_nzo_1"]}`)
		case "addfile":
			f, fh, err := r.FormFile("name")
			if err != nil || fh.Filename != "y.nzb" {
				t.Errorf("addfile %v", err)
			} else if b, _ := io.ReadAll(f); string(b) != "<nzb/>" {
				t.Errorf("content %q", b)
			}
			io.WriteString(w, `{"status": true, "nzo_ids": ["SABnzbd_nzo_2"]}`)
		case "get_cats":
			io.WriteString(w, `{"categories": ["*", "movies", "tv"]}`)
		case "queue":
			io.WriteString(w, `{"queue": {"status": "Downloading", "kbpersec": "1024", "slots": [
				{"nzo_id": "n1", "filename": "Movie", "status": "Downloading", "percentage": "25", "mb": "400", "mbleft": "300", "timeleft": "0:05:00", "cat": "movies"},
				{"nzo_id": "n2", "filename": "Other", "status": "Queued", "percentage": "0", "mb": "100", "mbleft": "100", "timeleft": "0:00:00", "cat": "*"}]}}`)
		case "history":
			io.WriteString(w, `{"history": {"slots": [
				{"nzo_id": "h1", "name": "Done", "status": "Completed", "bytes": 1000, "completed": 4102444800, "category": "tv"},
				{"nzo_id": "h2", "name": "Old", "status": "Completed", "bytes": 1, "completed": 1000}]}}`)
		default:
			t.Errorf("mode %s", q.Get("mode"))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	s := NewSABnzbd(srv.URL, "key")
	if err := s.Add(ctx, Item{Kind: KindNZBURL, URL: "https://n.example/x.nzb"}, "movies"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, Item{Kind: KindNZBFile, Filename: "y.nzb", Data: []byte("<nzb/>")}, ""); err != nil {
		t.Fatal(err)
	}
	if cats, _ := s.Categories(ctx); strings.Join(cats, ",") != "movies,tv" {
		t.Fatalf("cats %v", cats)
	}
	dl, err := s.Downloads(ctx)
	if err != nil || len(dl) != 3 {
		t.Fatalf("downloads %+v %v", dl, err)
	}
	if dl[0].Speed != 1024*1024 || dl[0].ETA != 300 || dl[0].Done != 100*1024*1024 || dl[0].Category != "movies" || dl[1].Speed != 0 || dl[1].Category != "" || dl[2].State != "done" {
		t.Fatalf("queue %+v", dl)
	}
	if err := NewSABnzbd(srv.URL, "bad").Add(ctx, Item{Kind: KindNZBURL, URL: "https://x/y.nzb"}, ""); err == nil || !strings.Contains(err.Error(), "API Key Incorrect") {
		t.Fatalf("bad key: %v", err)
	}
}

// fakeJD is a JDownloader local API with a link grabber.
type fakeJD struct {
	t          *testing.T
	mu         sync.Mutex
	added      []map[string]any
	collecting int  // isCollecting calls that still answer true
	empty      bool // the link grabber starts empty; addLinks fills it
	moved      []int64
	removed    []int64
	removedP   []int64
}

const fakeGrabber = `[
	{"uuid": 11, "packageUUID": 10, "name": "a.jpg", "bytesTotal": 100, "availability": "ONLINE"},
	{"uuid": 12, "packageUUID": 10, "name": "b.png", "bytesTotal": 100, "availability": "ONLINE"},
	{"uuid": 21, "packageUUID": 20, "name": "movie.mp4", "bytesTotal": 9000, "availability": "ONLINE"},
	{"uuid": 22, "packageUUID": 20, "name": "thumb.jpg", "bytesTotal": 10, "availability": "ONLINE"},
	{"uuid": 23, "packageUUID": 20, "name": "page.html", "bytesTotal": 5, "availability": "ONLINE"},
	{"uuid": 24, "packageUUID": 20, "name": "gone.mkv", "availability": "OFFLINE"}]`

func (f *fakeJD) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var params []json.RawMessage
		for _, p := range strings.Split(r.URL.RawQuery, "&") {
			if p == "" {
				continue
			}
			s, _ := url.QueryUnescape(p)
			params = append(params, json.RawMessage(s))
		}
		arg := func(i int, v any) {
			if err := json.Unmarshal(params[i], v); err != nil {
				f.t.Errorf("%s param %d %q: %v", r.URL.Path, i, params[i], err)
			}
		}
		switch r.URL.Path {
		case "/linkgrabberv2/addLinks":
			var q map[string]any
			arg(0, &q)
			f.added = append(f.added, q)
			f.empty = false
			io.WriteString(w, `{"data": {"id": 77}}`)
		case "/linkgrabberv2/addContainer":
			var typ, data string
			arg(0, &typ)
			arg(1, &data)
			if typ != "DLC" || !strings.HasPrefix(data, "data:application/octet-stream;base64,") {
				f.t.Errorf("container %q", params)
			}
			io.WriteString(w, `{"data": {"id": 78}}`)
		case "/linkgrabberv2/isCollecting":
			fmt.Fprintf(w, `{"data": %v}`, f.collecting > 0)
			f.collecting--
		case "/linkgrabberv2/queryPackages":
			io.WriteString(w, `{"data": [{"uuid": 10, "name": "Album"}, {"uuid": 20, "name": "Video page"}]}`)
		case "/linkgrabberv2/queryLinks":
			if f.empty {
				io.WriteString(w, `{"data": []}`)
			} else {
				io.WriteString(w, `{"data": `+fakeGrabber+`}`)
			}
		case "/linkgrabberv2/moveToDownloadlist":
			var l, p []int64
			arg(0, &l)
			arg(1, &p)
			f.moved = append(f.moved, l...)
			io.WriteString(w, `{"data": null}`)
		case "/linkgrabberv2/removeLinks":
			var l, p []int64
			arg(0, &l)
			arg(1, &p)
			f.removed = append(f.removed, l...)
			f.removedP = append(f.removedP, p...)
			io.WriteString(w, `{"data": null}`)
		case "/downloadsV2/queryPackages":
			io.WriteString(w, `{"data": [
				{"uuid": 1, "name": "Run", "bytesLoaded": 30, "bytesTotal": 120, "speed": 7, "eta": 13, "running": true, "enabled": true},
				{"uuid": 2, "name": "Fin", "bytesLoaded": 5, "bytesTotal": 5, "finished": true, "enabled": true},
				{"uuid": 3, "name": "Off", "bytesTotal": 5, "enabled": false}]}`)
		default:
			f.t.Errorf("path %s", r.URL.Path)
		}
	})
}

func TestJDownloader(t *testing.T) {
	f := &fakeJD{t: t}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	ctx := context.Background()
	j := NewJDownloader(srv.URL)
	if err := j.Add(ctx, Item{Kind: KindLink, URL: "https://h.example/f?a=1&b=2", Review: true}, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.Add(ctx, Item{Kind: KindDLCFile, Filename: "c.dlc", Data: []byte{1, 2}, Review: true}, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 1 || f.added[0]["links"] != "https://h.example/f?a=1&b=2" || f.added[0]["autostart"] != false {
		t.Fatalf("added %v", f.added)
	}
	dl, err := j.Downloads(ctx)
	if err != nil || len(dl) != 3 {
		t.Fatalf("downloads %+v %v", dl, err)
	}
	if dl[0].State != "downloading" || dl[0].Progress != 0.25 || dl[1].State != "paused" || dl[2].State != "done" {
		t.Fatalf("states %+v", dl)
	}

	pkgs, crawling, err := j.Review(ctx)
	if err != nil || crawling || len(pkgs) != 2 {
		t.Fatalf("review %+v %v %v", pkgs, crawling, err)
	}
	sel := func(p ReviewPackage) string {
		var s []string
		for _, l := range p.Links {
			if l.Selected {
				s = append(s, l.Name)
			}
		}
		return strings.Join(s, ",")
	}
	// An album: its images. A page with a video: only the video (not the
	// thumbnail, the page, or the offline file).
	if sel(pkgs[0]) != "a.jpg,b.png" || sel(pkgs[1]) != "movie.mp4" || len(pkgs[1].Links) != 4 || pkgs[0].Auto {
		t.Fatalf("selection %q %q", sel(pkgs[0]), sel(pkgs[1]))
	}
}

func TestJDownloaderAutoStart(t *testing.T) {
	f := &fakeJD{t: t, collecting: 2, empty: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	j := NewJDownloader(srv.URL)
	j.pollEvery = 10 * time.Millisecond
	j.settleTime = 50 * time.Millisecond
	if err := j.Add(context.Background(), Item{Kind: KindLink, URL: "https://bunkr.example/v/x"}, ""); err != nil {
		t.Fatal(err)
	}
	j.autoMu.Lock() // held by the automatic start until it is done
	defer j.autoMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.added[0]["autostart"] != false || fmt.Sprint(f.moved) != "[11 12 21]" || fmt.Sprint(f.removed) != "[22 23 24]" {
		t.Fatalf("added %v moved %v removed %v", f.added, f.moved, f.removed)
	}
	if len(j.pending) != 0 {
		t.Fatal("snapshot still pending")
	}
}

func TestPickRelevant(t *testing.T) {
	links := func(names ...string) []ReviewLink {
		var l []ReviewLink
		for _, n := range names {
			l = append(l, ReviewLink{Name: n, Availability: "ONLINE"})
		}
		return l
	}
	for names, want := range map[string]string{
		"a.jpg b.webp":                  "true true",
		"v.mkv t.jpg i.html":            "true false false",
		"part1.rar part2.rar cover.png": "true true false",
		"file_without_ext x.jpg":        "true false",
		"index.html":                    "false",
	} {
		got := fmt.Sprint(pickRelevant(links(strings.Fields(names)...)))
		if got != "["+want+"]" {
			t.Errorf("%s: %s, want [%s]", names, got, want)
		}
	}
}

// fakeClient records what it is given.
type fakeClient struct {
	added []string
	cats  []string
}

func (f *fakeClient) Add(_ context.Context, it Item, cat string) error {
	f.added = append(f.added, string(it.Kind)+"|"+it.Label()+"|"+cat)
	return nil
}
func (f *fakeClient) Downloads(context.Context) ([]Download, error) {
	return []Download{{Name: "x"}}, nil
}
func (f *fakeClient) Categories(context.Context) ([]string, error) { return f.cats, nil }

func TestDropHandler(t *testing.T) {
	jd, del, sab := &fakeClient{}, &fakeClient{cats: []string{"tv"}}, &fakeClient{}
	s := &server{clients: map[Target]Client{TargetJDownloader: jd, TargetDeluge: del, TargetSABnzbd: sab}}
	h := s.routes()

	form := func(text, target, cat string, files map[string]string) (*bytes.Buffer, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		w.WriteField("text", text)
		w.WriteField("target", target)
		w.WriteField("category", cat)
		for n, c := range files {
			p, _ := w.CreateFormFile("files", n)
			p.Write([]byte(c))
		}
		w.Close()
		return &b, w.FormDataContentType()
	}
	body, ct := form("get https://h.example/a and magnet:?xt=urn:btih:1 plus https://n.example/b.nzb", "auto", "tv",
		map[string]string{"x.torrent": "t", "notes.txt": "n"})

	req := httptest.NewRequest(http.MethodPost, "/api/drop", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without X-Drop: %d", rec.Code)
	}

	body, ct = form("get https://h.example/a and magnet:?xt=urn:btih:1 plus https://n.example/b.nzb", "auto", "tv",
		map[string]string{"x.torrent": "t", "notes.txt": "n"})
	req = httptest.NewRequest(http.MethodPost, "/api/drop", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Drop", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct{ Results []Result }
	json.NewDecoder(rec.Body).Decode(&out)
	if rec.Code != 200 || len(out.Results) != 5 {
		t.Fatalf("%d %+v", rec.Code, out)
	}
	// Auto routing ignores the category (it was picked for one client).
	if strings.Join(jd.added, ";") != "link|https://h.example/a|" ||
		strings.Join(del.added, ";") != "magnet|magnet:?xt=urn:btih:1|;torrent-file|x.torrent|" ||
		strings.Join(sab.added, ";") != "nzb-url|https://n.example/b.nzb|" {
		t.Fatalf("jd %v deluge %v sab %v", jd.added, del.added, sab.added)
	}
	if out.Results[0].OK || !strings.Contains(out.Results[0].Error, "unsupported") {
		t.Fatalf("txt file: %+v", out.Results[0])
	}

	// An explicit target takes the category; a magnet can't go to SABnzbd.
	body, ct = form("https://h.example/c.torrent magnet:?xt=urn:btih:2", "sabnzbd", "movies", nil)
	req = httptest.NewRequest(http.MethodPost, "/api/drop", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Drop", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	json.NewDecoder(rec.Body).Decode(&out)
	if len(out.Results) != 2 || out.Results[0].OK || out.Results[1].OK || !strings.Contains(out.Results[1].Error, "can't take") || len(sab.added) != 1 {
		t.Fatalf("explicit sabnzbd: %+v %v", out.Results, sab.added)
	}

	// An explicit target passes its category on.
	body, ct = form("https://h.example/d.nzb", "sabnzbd", "movies", nil)
	req = httptest.NewRequest(http.MethodPost, "/api/drop", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Drop", "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if sab.added[1] != "nzb-url|https://h.example/d.nzb|movies" {
		t.Fatalf("category: %v", sab.added)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if !strings.Contains(rec.Body.String(), `"categories":["tv"]`) {
		t.Fatalf("config %s", rec.Body)
	}
}

func TestSABnzbdErrorHidesKey(t *testing.T) {
	err := NewSABnzbd("http://127.0.0.1:1", "secretkey").Add(context.Background(), Item{Kind: KindNZBURL, URL: "https://x/y.nzb"}, "")
	if err == nil || strings.Contains(err.Error(), "secretkey") {
		t.Fatalf("error leaks the key: %v", err)
	}
}

func TestReviewEndpoints(t *testing.T) {
	f := &fakeJD{t: t}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	s := &server{clients: map[Target]Client{TargetJDownloader: NewJDownloader(srv.URL)}}
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/review", nil))
	if !strings.Contains(rec.Body.String(), `"name":"movie.mp4"`) || !strings.Contains(rec.Body.String(), `"id":"20"`) {
		t.Fatalf("review %s", rec.Body)
	}

	post := func(path, body string, xdrop bool) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if xdrop {
			req.Header.Set("X-Drop", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post("/api/review/start", `{"start":["21"],"remove":["22"]}`, false); c != http.StatusForbidden {
		t.Fatalf("without X-Drop: %d", c)
	}
	if c := post("/api/review/start", `{"start":["21"],"remove":["22","23"]}`, true); c != 200 || fmt.Sprint(f.moved) != "[21]" || fmt.Sprint(f.removed) != "[22 23]" {
		t.Fatalf("start %d %v %v", c, f.moved, f.removed)
	}
	if c := post("/api/review/discard", `{"packages":["10"]}`, true); c != 200 || fmt.Sprint(f.removedP) != "[10]" {
		t.Fatalf("discard %d %v", c, f.removedP)
	}
	if c := post("/api/review/start", `{"start":["x"]}`, true); c != http.StatusBadRequest {
		t.Fatalf("bad id: %d", c)
	}
}
