package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SABnzbd talks to SABnzbd's API (<url>/api) with its API key.
type SABnzbd struct {
	url    string
	apiKey string
	http   *http.Client
}

func NewSABnzbd(baseURL, apiKey string) *SABnzbd {
	return &SABnzbd{url: strings.TrimRight(baseURL, "/") + "/api", apiKey: apiKey, http: &http.Client{Timeout: httpTimeout}}
}

// do runs req and decodes the JSON answer into out.
func (s *SABnzbd) do(ctx context.Context, req *http.Request, out any) error {
	resp, err := s.http.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("sabnzbd: %w", stripURL(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sabnzbd: HTTP %d", resp.StatusCode)
	}
	// Errors (wrong key, bad request) come back as {"status": false, "error": "..."}.
	var e struct {
		Status *bool  `json:"status"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return errors.New("sabnzbd: " + e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("sabnzbd: %w", err)
		}
	}
	return nil
}

func (s *SABnzbd) query(mode string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	params.Set("mode", mode)
	params.Set("apikey", s.apiKey)
	params.Set("output", "json")
	return s.url + "?" + params.Encode()
}

func (s *SABnzbd) Add(ctx context.Context, it Item, category string) error {
	params := url.Values{}
	if category != "" {
		params.Set("cat", category)
	}
	var req *http.Request
	var err error
	switch it.Kind {
	case KindNZBURL, KindLink:
		params.Set("name", it.URL)
		req, err = http.NewRequest(http.MethodGet, s.query("addurl", params), nil)
	case KindNZBFile:
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, _ := w.CreateFormFile("name", it.Filename)
		part.Write(it.Data)
		w.Close()
		params.Set("nzbname", strings.TrimSuffix(it.Filename, ".nzb"))
		req, err = http.NewRequest(http.MethodPost, s.query("addfile", params), &body)
		if err == nil {
			req.Header.Set("Content-Type", w.FormDataContentType())
		}
	default:
		return fmt.Errorf("SABnzbd can't take a %s", it.Kind)
	}
	if err != nil {
		return err
	}
	var out struct {
		Status bool     `json:"status"`
		IDs    []string `json:"nzo_ids"`
	}
	if err := s.do(ctx, req, &out); err != nil {
		return err
	}
	if !out.Status {
		return errors.New("sabnzbd: not accepted")
	}
	return nil
}

func (s *SABnzbd) Categories(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequest(http.MethodGet, s.query("get_cats", nil), nil)
	var out struct {
		Categories []string `json:"categories"`
	}
	if err := s.do(ctx, req, &out); err != nil {
		return nil, err
	}
	var cats []string
	for _, c := range out.Categories {
		if c != "*" { // "*" is "no category"
			cats = append(cats, c)
		}
	}
	return cats, nil
}

// historyWindow is how long finished jobs stay in the status view.
const historyWindow = 24 * time.Hour

func (s *SABnzbd) Downloads(ctx context.Context) ([]Download, error) {
	req, _ := http.NewRequest(http.MethodGet, s.query("queue", url.Values{"limit": {"100"}}), nil)
	var q struct {
		Queue struct {
			Status   string `json:"status"` // Downloading, Paused, Idle
			KBPerSec string `json:"kbpersec"`
			Slots    []struct {
				ID         string `json:"nzo_id"`
				Filename   string `json:"filename"`
				Status     string `json:"status"`
				Percentage string `json:"percentage"`
				MB         string `json:"mb"`
				MBLeft     string `json:"mbleft"`
				TimeLeft   string `json:"timeleft"` // h:mm:ss
				Cat        string `json:"cat"`
			} `json:"slots"`
		} `json:"queue"`
	}
	if err := s.do(ctx, req, &q); err != nil {
		return nil, err
	}
	var out []Download
	speed := int64(atof(q.Queue.KBPerSec) * 1024)
	for i, sl := range q.Queue.Slots {
		size := int64(atof(sl.MB) * 1024 * 1024)
		left := int64(atof(sl.MBLeft) * 1024 * 1024)
		d := Download{Source: TargetSABnzbd, ID: sl.ID, Name: sl.Filename, State: sabState(sl.Status), Detail: sl.Status,
			Progress: atof(sl.Percentage) / 100, Size: size, Done: size - left, ETA: parseClock(sl.TimeLeft), Category: catOrEmpty(sl.Cat)}
		// SABnzbd downloads one job at a time; the overall speed is its speed.
		if i == 0 && d.State == "downloading" {
			d.Speed = speed
		}
		out = append(out, d)
	}

	req, _ = http.NewRequest(http.MethodGet, s.query("history", url.Values{"limit": {"20"}}), nil)
	var h struct {
		History struct {
			Slots []struct {
				ID        string `json:"nzo_id"`
				Name      string `json:"name"`
				Status    string `json:"status"` // Completed, Failed, Extracting, Verifying, ...
				Bytes     int64  `json:"bytes"`
				Completed int64  `json:"completed"` // unix time
				Cat       string `json:"category"`
				Fail      string `json:"fail_message"`
			} `json:"slots"`
		} `json:"history"`
	}
	if err := s.do(ctx, req, &h); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-historyWindow).Unix()
	for _, sl := range h.History.Slots {
		if sl.Completed != 0 && sl.Completed < cutoff {
			continue
		}
		state, detail := "checking", sl.Status // post-processing
		switch sl.Status {
		case "Completed":
			state = "done"
		case "Failed":
			state, detail = "error", sl.Fail
		}
		out = append(out, Download{Source: TargetSABnzbd, ID: sl.ID, Name: sl.Name, State: state, Detail: detail,
			Progress: 1, Size: sl.Bytes, Done: sl.Bytes, ETA: -1, Category: catOrEmpty(sl.Cat)})
	}
	return out, nil
}

func sabState(s string) string {
	switch s {
	case "Downloading", "Fetching", "Grabbing":
		return "downloading"
	case "Paused":
		return "paused"
	case "Queued":
		return "queued"
	case "Checking", "QuickCheck", "Verifying", "Repairing", "Extracting", "Moving", "Running":
		return "checking"
	}
	return strings.ToLower(s)
}

func catOrEmpty(c string) string {
	if c == "*" || c == "None" {
		return ""
	}
	return c
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// parseClock turns SABnzbd's "h:mm:ss" (or "d:hh:mm:ss") into seconds, -1
// if it doesn't parse.
func parseClock(s string) int64 {
	parts := strings.Split(s, ":")
	var total int64
	mult := []int64{1, 60, 3600, 86400}
	if len(parts) == 0 || len(parts) > len(mult) {
		return -1
	}
	for i := range parts {
		n, err := strconv.ParseInt(parts[len(parts)-1-i], 10, 64)
		if err != nil {
			return -1
		}
		total += n * mult[i]
	}
	return total
}
