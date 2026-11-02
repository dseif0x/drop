// drop takes links, magnets, .torrent/.nzb/.dlc files and hands each to the
// right download client in the cluster: JDownloader for hoster and other
// links, Deluge for torrents, SABnzbd for NZBs. It also shows what the
// three are downloading. It has no login of its own: it runs behind the
// cluster's forward-auth login (tinyauth).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	httpTimeout   = 20 * time.Second
	maxUpload     = 32 << 20 // whole request
	statusTimeout = 8 * time.Second
)

type server struct {
	clients map[Target]Client
	static  string
}

func main() {
	clients := map[Target]Client{}
	if u := os.Getenv("JDOWNLOADER_URL"); u != "" {
		clients[TargetJDownloader] = NewJDownloader(u)
	}
	if u := os.Getenv("DELUGE_URL"); u != "" {
		clients[TargetDeluge] = NewDeluge(u, os.Getenv("DELUGE_PASSWORD"))
	}
	if u := os.Getenv("SABNZBD_URL"); u != "" {
		clients[TargetSABnzbd] = NewSABnzbd(u, os.Getenv("SABNZBD_API_KEY"))
	}
	if len(clients) == 0 {
		log.Print("no download client configured (JDOWNLOADER_URL, DELUGE_URL, SABNZBD_URL)")
	}
	s := &server{clients: clients, static: envOr("STATIC_DIR", "./static")}
	addr := ":" + envOr("PORT", "8080")
	log.Printf("drop listening on %s, clients: %v", addr, s.targets())
	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) targets() []Target {
	var t []Target
	for k := range s.clients {
		t = append(t, k)
	}
	sort.Slice(t, func(i, j int) bool { return t[i] < t[j] })
	return t
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/drop", s.handleDrop)
	mux.HandleFunc("GET /api/review", s.handleReview)
	mux.HandleFunc("POST /api/review/start", s.handleReviewStart)
	mux.HandleFunc("POST /api/review/discard", s.handleReviewDiscard)
	mux.HandleFunc("GET /", s.handleStatic)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// handleConfig tells the page which clients exist and their categories.
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	type clientInfo struct {
		Target     Target   `json:"target"`
		Categories []string `json:"categories"`
		Error      string   `json:"error,omitempty"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), statusTimeout)
	defer cancel()
	out := make([]clientInfo, 0, len(s.clients))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for t, c := range s.clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ci := clientInfo{Target: t, Categories: []string{}}
			cats, err := c.Categories(ctx)
			if err != nil {
				ci.Error = err.Error()
			} else if cats != nil {
				ci.Categories = cats
			}
			mu.Lock()
			out = append(out, ci)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

// handleStatus collects every client's downloads; a client that fails is
// reported in errors, the others still show.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), statusTimeout)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	downloads := []Download{}
	errs := map[Target]string{}
	for t, c := range s.clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := c.Downloads(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[t] = err.Error()
				return
			}
			downloads = append(downloads, d...)
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"downloads": downloads, "errors": errs})
}

// Result is what happened to one dropped item.
type Result struct {
	Item   string `json:"item"`
	Target Target `json:"target,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// handleDrop takes a multipart form: text (links anywhere in it), files,
// target ("auto" or a client), category (for Deluge/SABnzbd) and review
// ("0" lets drop start JDownloader's relevant files itself; anything else
// leaves them in the link grabber for the review list).
//
// It only answers requests carrying the X-Drop header: a cross-site form
// can't set it, so another page can't make a logged-in browser add
// downloads.
func (s *server) handleDrop(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Drop") == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing X-Drop header"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad form: " + err.Error()})
		return
	}
	target := Target(r.FormValue("target"))
	category := r.FormValue("category")
	review := r.FormValue("review") != "0"

	var items []Item
	var results []Result
	for _, l := range extractLinks(r.FormValue("text")) {
		items = append(items, Item{Kind: classifyLink(l), URL: l})
	}
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["files"] {
			kind, ok := classifyFile(fh.Filename)
			if !ok {
				results = append(results, Result{Item: fh.Filename, Error: "unsupported file type (.torrent, .nzb or .dlc)"})
				continue
			}
			f, err := fh.Open()
			if err != nil {
				results = append(results, Result{Item: fh.Filename, Error: err.Error()})
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				results = append(results, Result{Item: fh.Filename, Error: err.Error()})
				continue
			}
			items = append(items, Item{Kind: kind, Filename: filepath.Base(fh.Filename), Data: data})
		}
	}
	if len(items) == 0 && len(results) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no links or files found"})
		return
	}

	for _, it := range items {
		it.Review = review
		results = append(results, s.add(r.Context(), it, target, category))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// add sends one item to its client: the chosen target, or the automatic one.
// The category only applies to the client it was picked for.
func (s *server) add(ctx context.Context, it Item, target Target, category string) Result {
	t := target
	if t == "" || t == "auto" {
		t = autoTarget(it.Kind)
	}
	res := Result{Item: it.Label(), Target: t}
	c, ok := s.clients[t]
	switch {
	case !ok:
		res.Error = string(t) + " is not configured"
	case !accepts(t, it.Kind):
		res.Error = string(t) + " can't take a " + string(it.Kind)
	default:
		cat := category
		if target == "" || target == "auto" {
			cat = "" // a category is chosen for one client; auto-routed items may go elsewhere
		}
		err := c.Add(ctx, it, cat)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
		}
	}
	log.Printf("drop %s -> %s: ok=%v %s", it.Kind, t, res.OK, res.Error)
	return res
}

// Reviewer is a client with a review list (JDownloader's link grabber).
type Reviewer interface {
	Review(ctx context.Context) ([]ReviewPackage, bool, error)
	Start(ctx context.Context, start, remove []int64) error
	Discard(ctx context.Context, packages []int64) error
}

func (s *server) reviewer() Reviewer {
	r, _ := s.clients[TargetJDownloader].(Reviewer)
	return r
}

// handleReview lists the packages waiting in JDownloader's link grabber.
func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	rv := s.reviewer()
	if rv == nil {
		writeJSON(w, http.StatusOK, map[string]any{"packages": []ReviewPackage{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), statusTimeout)
	defer cancel()
	pkgs, crawling, err := rv.Review(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"packages": []ReviewPackage{}, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"packages": pkgs, "crawling": crawling})
}

// reviewAction decodes a review request body after the same X-Drop check as
// /api/drop.
func (s *server) reviewAction(w http.ResponseWriter, r *http.Request, body any) (Reviewer, bool) {
	if r.Header.Get("X-Drop") == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing X-Drop header"})
		return nil, false
	}
	rv := s.reviewer()
	if rv == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "jdownloader is not configured"})
		return nil, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return nil, false
	}
	return rv, true
}

// ids parses JDownloader ids (sent as strings: they don't fit a JS number
// safely in every case).
func ids(in []string) ([]int64, error) {
	out := make([]int64, 0, len(in))
	for _, s := range in {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// handleReviewStart downloads the chosen links and drops the rest.
func (s *server) handleReviewStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Start  []string `json:"start"`
		Remove []string `json:"remove"`
	}
	rv, ok := s.reviewAction(w, r, &body)
	if !ok {
		return
	}
	start, err1 := ids(body.Start)
	remove, err2 := ids(body.Remove)
	if err := errors.Join(err1, err2); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := rv.Start(r.Context(), start, remove); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("review: started %d links, removed %d", len(start), len(remove))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleReviewDiscard removes whole packages from the link grabber.
func (s *server) handleReviewDiscard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Packages []string `json:"packages"`
	}
	rv, ok := s.reviewAction(w, r, &body)
	if !ok {
		return
	}
	pkgs, err := ids(body.Packages)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := rv.Discard(r.Context(), pkgs); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStatic serves the built frontend; unknown paths get index.html (the
// page has a single route, plus /share for the PWA share target).
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := filepath.Clean("/" + r.URL.Path)
	full := filepath.Join(s.static, p)
	if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
		if strings.HasSuffix(p, "/sw.js") || strings.HasSuffix(p, ".webmanifest") || p == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(p, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(w, r, full)
		return
	}
	index := filepath.Join(s.static, "index.html")
	if _, err := os.Stat(index); errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}
