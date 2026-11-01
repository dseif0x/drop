package main

import (
	"slices"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	text := `Here: https://example.com/a.zip, and (https://rapidgator.net/file/abc).
magnet:?xt=urn:btih:ABC&dn=x  https://example.com/a.zip
<a href="https://x.org/y.nzb">nzb</a> not-a-link http//broken`
	got := extractLinks(text)
	want := []string{
		"https://example.com/a.zip",
		"https://rapidgator.net/file/abc",
		"magnet:?xt=urn:btih:ABC&dn=x",
		"https://x.org/y.nzb",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestClassify(t *testing.T) {
	for s, want := range map[string]Kind{
		"magnet:?xt=urn:btih:abc":               KindMagnet,
		"MAGNET:?xt=urn:btih:abc":               KindMagnet,
		"https://t.example/dl/file.torrent":     KindTorrentURL,
		"https://t.example/dl/file.TORRENT?k=1": KindTorrentURL,
		"https://n.example/get/x.nzb":           KindNZBURL,
		"https://youtube.com/watch?v=1":         KindLink,
		"https://host.example/f/x.torrent.zip":  KindLink,
	} {
		if got := classifyLink(s); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
	for name, want := range map[string]Kind{"a.torrent": KindTorrentFile, "b.NZB": KindNZBFile, "c.dlc": KindDLCFile} {
		if got, ok := classifyFile(name); !ok || got != want {
			t.Errorf("%s: %s %v", name, got, ok)
		}
	}
	if _, ok := classifyFile("x.zip"); ok {
		t.Error("x.zip accepted")
	}
	if autoTarget(KindMagnet) != TargetDeluge || autoTarget(KindNZBFile) != TargetSABnzbd || autoTarget(KindLink) != TargetJDownloader {
		t.Error("auto targets")
	}
	if accepts(TargetSABnzbd, KindMagnet) || accepts(TargetJDownloader, KindTorrentFile) || !accepts(TargetDeluge, KindLink) {
		t.Error("accepts")
	}
}
