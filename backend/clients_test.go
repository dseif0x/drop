package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

func TestJDownloader(t *testing.T) {
	var added []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var params []any
		for _, p := range strings.Split(r.URL.RawQuery, "&") {
			if p == "" {
				continue
			}
			s, _ := url.QueryUnescape(p)
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				t.Errorf("param %q: %v", s, err)
			}
			params = append(params, v)
		}
		switch r.URL.Path {
		case "/linkgrabberv2/addLinks":
			q := params[0].(map[string]any)
			added = append(added, q["links"].(string))
			if q["autostart"] != true {
				t.Error("autostart")
			}
			io.WriteString(w, `{"data": {"id": 1}}`)
		case "/linkgrabberv2/addContainer":
			if params[0] != "DLC" || !strings.HasPrefix(params[1].(string), "data:application/octet-stream;base64,") {
				t.Errorf("container %v", params)
			}
			io.WriteString(w, `{"data": {"id": 2}}`)
		case "/downloadsV2/queryPackages":
			io.WriteString(w, `{"data": [
				{"uuid": 1, "name": "Run", "bytesLoaded": 30, "bytesTotal": 120, "speed": 7, "eta": 13, "running": true, "enabled": true},
				{"uuid": 2, "name": "Fin", "bytesLoaded": 5, "bytesTotal": 5, "finished": true, "enabled": true},
				{"uuid": 3, "name": "Off", "bytesTotal": 5, "enabled": false}]}`)
		case "/linkgrabberv2/queryPackages":
			io.WriteString(w, `[{"uuid": 9, "name": "Crawl", "bytesTotal": 0}]`)
		default:
			t.Errorf("path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	j := NewJDownloader(srv.URL)
	if err := j.Add(ctx, Item{Kind: KindLink, URL: "https://h.example/f?a=1&b=2"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := j.Add(ctx, Item{Kind: KindDLCFile, Filename: "c.dlc", Data: []byte{1, 2}}, ""); err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "https://h.example/f?a=1&b=2" {
		t.Fatalf("added %q", added)
	}
	dl, err := j.Downloads(ctx)
	if err != nil || len(dl) != 4 {
		t.Fatalf("downloads %+v %v", dl, err)
	}
	if dl[0].State != "downloading" || dl[0].Progress != 0.25 || dl[1].State != "paused" || dl[2].State != "crawling" || dl[3].State != "done" {
		t.Fatalf("states %+v", dl)
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
