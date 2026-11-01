package main

import (
	"context"
	"errors"
	"net/url"
)

// Download is one entry of the status view, the same shape for every client.
type Download struct {
	Source   Target  `json:"source"`
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	State    string  `json:"state"`            // downloading, queued, paused, seeding, done, checking, crawling, error
	Detail   string  `json:"detail,omitempty"` // the client's own wording of the state
	Progress float64 `json:"progress"`         // 0..1
	Size     int64   `json:"size"`             // bytes, 0 if unknown
	Done     int64   `json:"done"`             // bytes
	Speed    int64   `json:"speed"`            // bytes/s
	ETA      int64   `json:"eta"`              // seconds, -1 if unknown
	Category string  `json:"category,omitempty"`
}

// Client is a download client drop sends items to.
type Client interface {
	// Add hands over one item; category may be empty.
	Add(ctx context.Context, it Item, category string) error
	// Downloads lists what the client is working on (and what it finished
	// recently, where it keeps that).
	Downloads(ctx context.Context) ([]Download, error)
	// Categories lists the categories or labels items can be filed under;
	// nil if the client has none.
	Categories(ctx context.Context) ([]string, error)
}

// stripURL drops the request URL from a transport error: SABnzbd's carries
// the API key, and errors are shown in the page.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
