package feed

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Subscription is one feed described by an OPML file.
type Subscription struct {
	FeedURL  string
	SiteURL  string
	Title    string
	Category string
}

// opmlDocument mirrors the parts of the OPML format that carry subscriptions.
// Everything else — head metadata, ownership, expansion state — is ignored.
type opmlDocument struct {
	XMLName xml.Name      `xml:"opml"`
	Body    opmlOutlineML `xml:"body"`
}

type opmlOutlineML struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text    string `xml:"text,attr"`
	Title   string `xml:"title,attr"`
	Type    string `xml:"type,attr"`
	XMLURL  string `xml:"xmlUrl,attr"`
	HTMLURL string `xml:"htmlUrl,attr"`

	Outlines []opmlOutline `xml:"outline"`
}

// ParseOPML reads subscriptions from an OPML document.
//
// The format is loosely specified and every reader writes it slightly
// differently, so the parser is permissive about structure and strict only
// about what it needs: an outline with an xmlUrl is a feed, an outline without
// one is a folder whose name becomes the category of everything beneath it.
//
// Feed URLs are deliberately *not* canonicalized. Canonicalization is tuned
// for article links, where a stripped `ref` or `source` parameter is tracking
// noise; on a feed endpoint the same parameter may select which feed is
// served. The subscription URL is stored exactly as the exporting reader
// wrote it.
func ParseOPML(r io.Reader) ([]Subscription, error) {
	var doc opmlDocument

	decoder := xml.NewDecoder(r)
	// Real-world OPML is frequently mislabeled or emitted in a legacy
	// encoding; without a charset reader those files fail to parse at all.
	decoder.CharsetReader = charsetReader

	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing OPML: %w", err)
	}

	var subs []Subscription
	seen := make(map[string]bool)

	var walk func(outlines []opmlOutline, category string)
	walk = func(outlines []opmlOutline, category string) {
		for _, o := range outlines {
			feedURL := strings.TrimSpace(o.XMLURL)

			if feedURL == "" {
				// A folder. Its name qualifies everything below it; nested
				// folders are joined so that no level is silently discarded.
				walk(o.Outlines, joinCategory(category, outlineName(o)))
				continue
			}

			if !isFetchableURL(feedURL) {
				continue
			}
			if seen[feedURL] {
				continue
			}
			seen[feedURL] = true

			subs = append(subs, Subscription{
				FeedURL:  feedURL,
				SiteURL:  strings.TrimSpace(o.HTMLURL),
				Title:    subscriptionTitle(o, feedURL),
				Category: category,
			})

			// A feed outline with children is unusual but legal; the children
			// are still subscriptions.
			walk(o.Outlines, category)
		}
	}
	walk(doc.Body.Outlines, "")

	return subs, nil
}

// outlineName returns the display name of a folder outline.
//
// OPML says `text` is the required attribute and `title` is optional, but
// enough exporters populate only one of the two that both have to be tried.
func outlineName(o opmlOutline) string {
	if t := strings.TrimSpace(o.Text); t != "" {
		return t
	}
	return strings.TrimSpace(o.Title)
}

// subscriptionTitle prefers a human title and falls back to the URL, so that
// no feed is ever stored with an empty name.
func subscriptionTitle(o opmlOutline, feedURL string) string {
	for _, candidate := range []string{o.Title, o.Text} {
		if t := strings.TrimSpace(candidate); t != "" {
			return t
		}
	}
	return feedURL
}

func joinCategory(parent, child string) string {
	switch {
	case child == "":
		return parent
	case parent == "":
		return child
	default:
		return parent + "/" + child
	}
}

// isFetchableURL reports whether a subscription URL is one this service could
// poll. Anything else in an OPML file is a bookmark, not a feed.
func isFetchableURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// charsetReader handles the non-UTF-8 encodings that turn up in OPML exported
// by older readers.
//
// Only the encodings that are byte-compatible with what encoding/xml already
// handles are accepted. A genuinely different encoding is reported rather than
// silently mis-decoded into mojibake that would then be stored as feed titles
// forever.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return input, nil
	default:
		return nil, fmt.Errorf("unsupported OPML character encoding %q: re-export the file as UTF-8", charset)
	}
}
