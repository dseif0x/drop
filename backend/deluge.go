package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strings"
	"sync"
)

// Deluge talks to the Deluge web UI's JSON-RPC endpoint (<url>/json), logged
// in with the web UI password. The web UI is connected to its daemon (the
// first configured host) if it isn't yet.
type Deluge struct {
	url      string
	password string
	http     *http.Client

	mu     sync.Mutex
	nextID int
}

func NewDeluge(baseURL, password string) *Deluge {
	jar, _ := cookiejar.New(nil)
	return &Deluge{url: strings.TrimRight(baseURL, "/") + "/json", password: password,
		http: &http.Client{Timeout: httpTimeout, Jar: jar}}
}

type delugeError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func (e *delugeError) Error() string { return "deluge: " + e.Message }

// errNotAuthenticated is Deluge's code for a missing or expired session.
const delugeNotAuthenticated = 1

// rawCall does one JSON-RPC request.
func (d *Deluge) rawCall(ctx context.Context, method string, params []any, result any) error {
	d.mu.Lock()
	d.nextID++
	id := d.nextID
	d.mu.Unlock()
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"method": method, "params": params, "id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("deluge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deluge: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *delugeError    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("deluge: %w", err)
	}
	if out.Error != nil {
		return out.Error
	}
	if result != nil {
		return json.Unmarshal(out.Result, result)
	}
	return nil
}

// login authenticates the session and connects the web UI to the daemon.
func (d *Deluge) login(ctx context.Context) error {
	var ok bool
	if err := d.rawCall(ctx, "auth.login", []any{d.password}, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("deluge: wrong web UI password")
	}
	var connected bool
	if err := d.rawCall(ctx, "web.connected", nil, &connected); err != nil {
		return err
	}
	if connected {
		return nil
	}
	var hosts [][]any
	if err := d.rawCall(ctx, "web.get_hosts", nil, &hosts); err != nil {
		return err
	}
	if len(hosts) == 0 || len(hosts[0]) == 0 {
		return errors.New("deluge: the web UI has no daemon configured")
	}
	return d.rawCall(ctx, "web.connect", []any{hosts[0][0]}, nil)
}

// call does a request, logging in first when the session is missing or
// expired.
func (d *Deluge) call(ctx context.Context, method string, params []any, result any) error {
	err := d.rawCall(ctx, method, params, result)
	var de *delugeError
	if errors.As(err, &de) && de.Code == delugeNotAuthenticated {
		if err := d.login(ctx); err != nil {
			return err
		}
		return d.rawCall(ctx, method, params, result)
	}
	return err
}

func (d *Deluge) Add(ctx context.Context, it Item, label string) error {
	options := map[string]any{}
	var id string
	var err error
	switch it.Kind {
	case KindMagnet:
		err = d.call(ctx, "core.add_torrent_magnet", []any{it.URL, options}, &id)
	case KindTorrentURL, KindLink:
		err = d.call(ctx, "core.add_torrent_url", []any{it.URL, options}, &id)
	case KindTorrentFile:
		err = d.call(ctx, "core.add_torrent_file", []any{it.Filename, base64.StdEncoding.EncodeToString(it.Data), options}, &id)
	default:
		return fmt.Errorf("deluge can't take a %s", it.Kind)
	}
	if err != nil {
		return err
	}
	if id == "" {
		// Deluge returns no id for a torrent it already has.
		return errors.New("deluge: already added")
	}
	if label != "" {
		if err := d.call(ctx, "label.set_torrent", []any{id, label}, nil); err != nil {
			return fmt.Errorf("added, but setting the label failed: %w", err)
		}
	}
	return nil
}

func (d *Deluge) Categories(ctx context.Context) ([]string, error) {
	var labels []string
	if err := d.call(ctx, "label.get_labels", nil, &labels); err != nil {
		var de *delugeError
		if errors.As(err, &de) {
			return nil, nil // the Label plugin isn't enabled
		}
		return nil, err
	}
	sort.Strings(labels)
	return labels, nil
}

type delugeTorrent struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"` // percent
	Size     int64   `json:"total_wanted"`
	Done     int64   `json:"total_done"`
	Speed    int64   `json:"download_payload_rate"`
	ETA      float64 `json:"eta"`
	Label    string  `json:"label"`
	Added    float64 `json:"time_added"`
}

var delugeFields = []string{"name", "state", "progress", "total_wanted", "total_done", "download_payload_rate", "eta", "label", "time_added"}

func (d *Deluge) Downloads(ctx context.Context) ([]Download, error) {
	var torrents map[string]delugeTorrent
	if err := d.call(ctx, "core.get_torrents_status", []any{map[string]any{}, delugeFields}, &torrents); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(torrents))
	for id := range torrents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return torrents[ids[i]].Added > torrents[ids[j]].Added })
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		t := torrents[id]
		eta := int64(t.ETA)
		if eta <= 0 {
			eta = -1
		}
		state := delugeState(t.State)
		if state == "paused" && t.Progress >= 100 {
			state = "done" // finished and stopped (not seeding)
		}
		out = append(out, Download{Source: TargetDeluge, ID: id, Name: t.Name, State: state, Detail: t.State,
			Progress: t.Progress / 100, Size: t.Size, Done: t.Done, Speed: t.Speed, ETA: eta, Category: t.Label})
	}
	return out, nil
}

func delugeState(s string) string {
	switch s {
	case "Downloading":
		return "downloading"
	case "Seeding":
		return "seeding"
	case "Paused":
		return "paused"
	case "Queued":
		return "queued"
	case "Checking", "Allocating", "Moving":
		return "checking"
	case "Error":
		return "error"
	}
	return strings.ToLower(s)
}
