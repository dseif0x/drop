package main

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Target is a download client an item goes to.
type Target string

const (
	TargetJDownloader Target = "jdownloader"
	TargetDeluge      Target = "deluge"
	TargetSABnzbd     Target = "sabnzbd"
)

// Kind says what an item is, which decides how its target is called.
type Kind string

const (
	KindMagnet      Kind = "magnet"       // magnet: URI
	KindTorrentURL  Kind = "torrent-url"  // URL of a .torrent file
	KindNZBURL      Kind = "nzb-url"      // URL of an .nzb file
	KindLink        Kind = "link"         // any other http(s) link: hosters, video sites, ...
	KindTorrentFile Kind = "torrent-file" // uploaded .torrent
	KindNZBFile     Kind = "nzb-file"     // uploaded .nzb
	KindDLCFile     Kind = "dlc-file"     // uploaded JDownloader container
)

// Item is one thing to download: a link from the pasted text or an uploaded
// file.
type Item struct {
	Kind     Kind
	URL      string // links
	Filename string // files
	Data     []byte // files
}

// Label is how the item shows up in results: the link or the file name.
func (i Item) Label() string {
	if i.Filename != "" {
		return i.Filename
	}
	return i.URL
}

// linkPattern finds magnet URIs and http(s) URLs anywhere in pasted text:
// one per line, several per line, or inside other text. Trailing
// punctuation that usually belongs to the sentence, not the link, is trimmed
// in extractLinks.
var linkPattern = regexp.MustCompile(`(?i)(magnet:\?[^\s"'<>]+|https?://[^\s"'<>]+)`)

// extractLinks returns the links in text, in order, without duplicates.
func extractLinks(text string) []string {
	var links []string
	seen := map[string]bool{}
	for _, m := range linkPattern.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:!?)]}")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		links = append(links, m)
	}
	return links
}

// classifyLink says what kind of link s is.
func classifyLink(s string) Kind {
	if strings.HasPrefix(strings.ToLower(s), "magnet:") {
		return KindMagnet
	}
	u, err := url.Parse(s)
	if err != nil {
		return KindLink
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".torrent":
		return KindTorrentURL
	case ".nzb":
		return KindNZBURL
	}
	return KindLink
}

// classifyFile says what kind of upload name is; ok is false for files no
// client takes.
func classifyFile(name string) (Kind, bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".torrent":
		return KindTorrentFile, true
	case ".nzb":
		return KindNZBFile, true
	case ".dlc":
		return KindDLCFile, true
	}
	return "", false
}

// autoTarget is the client an item of kind k goes to.
func autoTarget(k Kind) Target {
	switch k {
	case KindMagnet, KindTorrentURL, KindTorrentFile:
		return TargetDeluge
	case KindNZBURL, KindNZBFile:
		return TargetSABnzbd
	}
	return TargetJDownloader
}

// accepts reports whether target t can take an item of kind k: an override
// can send a plain link to Deluge or SABnzbd (as a .torrent/.nzb URL) or a
// .torrent/.nzb URL to JDownloader, but files and magnets only go where they
// can be read.
func accepts(t Target, k Kind) bool {
	switch t {
	case TargetDeluge:
		return k == KindMagnet || k == KindTorrentURL || k == KindTorrentFile || k == KindLink
	case TargetSABnzbd:
		return k == KindNZBURL || k == KindNZBFile || k == KindLink
	case TargetJDownloader:
		return k == KindLink || k == KindTorrentURL || k == KindNZBURL || k == KindDLCFile
	}
	return false
}
