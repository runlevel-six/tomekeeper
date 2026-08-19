package feed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"

	"github.com/runlevel-six/tomekeeper/internal/httpclient"
)

// ErrNotAFeed means the URL answered, but with something that is not a feed.
var ErrNotAFeed = errors.New("not a feed")

// ErrNoFeedDiscovered means an HTML page was fetched successfully and advertises
// no feed.
var ErrNoFeedDiscovered = errors.New("no feed advertised on that page")

// Probed is what one look at a feed URL found.
//
// Everything here comes from the feed's own document. Nothing is stored, and the
// point is to put in front of somebody the things they need to decide whether this
// is the feed they meant: what it calls itself, how many items it carries, and how
// recently it was updated. A feed whose newest item is from 2019 is a fact worth
// knowing before subscribing, not after a week of empty polls.
type Probed struct {
	// FeedURL is the URL that actually served the feed, which is not always the
	// one that was typed: a site URL is followed to the feed it advertises.
	FeedURL string

	// Discovered records that FeedURL came from a link on an HTML page rather than
	// from what was typed. Worth showing, because the address being subscribed to
	// is then not the one the reader entered.
	Discovered bool

	Title       string
	Description string
	SiteURL     string

	Items  int
	Newest *time.Time

	// Sample is the first few item titles, which is the cheapest way for a person
	// to recognize a feed they know.
	Sample []string
}

// maxSample is how many item titles a probe reports.
const maxSample = 5

// Probe fetches a URL and reports what kind of feed is there, writing nothing.
//
// Two things make this different from a poll, and both are deliberate. It stores
// nothing — there is no feed row yet, which is the whole point — and it follows an
// HTML page to the feed that page advertises, because the URL a person has to hand
// is usually the site's, not its feed's. Making somebody hunt for the feed URL is
// the step that turns "subscribe to this" into a chore.
//
// robots.txt is skipped, exactly as polling skips it: a feed is published for
// automated consumption and this request is one a reader explicitly asked for.
func Probe(ctx context.Context, c *httpclient.Client, rawURL string) (Probed, error) {
	if c == nil {
		return Probed{}, errors.New("no HTTP client is configured, so a feed cannot be tested")
	}

	normalized, err := NormalizeFeedURL(rawURL)
	if err != nil {
		return Probed{}, err
	}

	body, err := fetchForProbe(ctx, c, normalized)
	if err != nil {
		return Probed{}, err
	}

	probed, err := parseProbe(body, normalized)
	if err == nil {
		return probed, nil
	}
	if !errors.Is(err, ErrNotAFeed) {
		return Probed{}, err
	}

	// Not a feed. If it is an HTML page advertising one, follow that — once. A
	// second hop would be following a link found on a page found by a link, which
	// is a crawl rather than a subscription.
	linked, ok := discoverFeedURL(body, normalized)
	if !ok {
		return Probed{}, ErrNoFeedDiscovered
	}

	body, err = fetchForProbe(ctx, c, linked)
	if err != nil {
		return Probed{}, fmt.Errorf("that page advertises %s, which could not be fetched: %w", linked, err)
	}

	probed, err = parseProbe(body, linked)
	if err != nil {
		return Probed{}, fmt.Errorf("that page advertises %s, which is not a feed: %w", linked, err)
	}
	probed.Discovered = true
	return probed, nil
}

// NormalizeFeedURL cleans up what somebody typed.
//
// A scheme is assumed because an address bar does not show one, and that is where
// the URL was copied from. Deliberately no canonicalization beyond this: a query
// parameter on a feed endpoint may select which feed is served, so the URL is
// stored and fetched exactly as given.
func NormalizeFeedURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("no address was given")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%q is not a web address", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s is not a web address", parsed.Scheme)
	}
	return parsed.String(), nil
}

func fetchForProbe(ctx context.Context, c *httpclient.Client, feedURL string) ([]byte, error) {
	resp, err := c.Do(ctx, httpclient.Request{URL: feedURL, SkipRobots: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}

	// The client's own response cap applies, which is where that judgment belongs:
	// a probe reads exactly what a poll would read.
	body, err := httpclient.ReadBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: the response was empty", ErrNotAFeed)
	}
	return body, nil
}

// parseProbe reads a feed document into a report.
func parseProbe(body []byte, feedURL string) (Probed, error) {
	// A parser per probe rather than one shared, matching the poller: gofeed's
	// Parser holds mutable state and is not safe for concurrent use.
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return Probed{}, fmt.Errorf("%w: %w", ErrNotAFeed, err)
	}

	probed := Probed{
		FeedURL:     feedURL,
		Title:       strings.TrimSpace(parsed.Title),
		Description: strings.TrimSpace(parsed.Description),
		SiteURL:     strings.TrimSpace(parsed.Link),
		Items:       len(parsed.Items),
	}

	for _, item := range parsed.Items {
		if len(probed.Sample) < maxSample {
			if title := strings.TrimSpace(item.Title); title != "" {
				probed.Sample = append(probed.Sample, title)
			}
		}
		if at := itemPublished(item); at != nil {
			if probed.Newest == nil || at.After(*probed.Newest) {
				probed.Newest = at
			}
		}
	}

	return probed, nil
}

// discoverFeedURL finds the feed an HTML page advertises.
//
// The first RSS or Atom alternate link wins. Sites commonly advertise several — a
// comments feed, a per-category feed — and the first is conventionally the main
// one; guessing better than that would mean ranking somebody's markup, and being
// wrong quietly is worse than being simple.
func discoverFeedURL(body []byte, pageURL string) (string, bool) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", false
	}

	base, err := url.Parse(pageURL)
	if err != nil {
		return "", false
	}

	var found string
	doc.Find(`link[rel="alternate"]`).EachWithBreak(func(_ int, link *goquery.Selection) bool {
		switch strings.ToLower(strings.TrimSpace(link.AttrOr("type", ""))) {
		case "application/rss+xml", "application/atom+xml", "application/feed+json", "application/json":
		default:
			return true
		}

		href := strings.TrimSpace(link.AttrOr("href", ""))
		if href == "" {
			return true
		}
		ref, err := url.Parse(href)
		if err != nil {
			return true
		}
		found = base.ResolveReference(ref).String()
		return false
	})

	return found, found != ""
}
