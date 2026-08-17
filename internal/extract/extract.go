// Package extract turns a fetched HTML page into a readable article body.
//
// Extraction is a *derived, versioned view* over the stored raw fetch, never
// the authoritative copy (principle 2.2). Extraction quality only improves, so
// every body carries the name and version of what produced it and can be
// regenerated in bulk by `tome reextract` without re-fetching anything.
//
// The ladder runs in order and stops at the first acceptable result:
//
//  1. A domain rule's CSS selector, when one exists for the host.
//  2. go-trafilatura, the primary extractor.
//  3. go-readability, the fallback.
//  4. Headless rendering — M8, for domains flagged requires_js.
//  5. The feed's own body, when everything else failed.
//
// Rung 4 is deliberately absent here: nothing in this package starts a
// browser. When M8 lands it becomes another Extractor implementation.
package extract

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"
	"github.com/markusmobius/go-trafilatura"
	"golang.org/x/net/html"
)

// Version is the extraction behavior version, recorded on every content row.
//
// **Bump this whenever extraction output changes** — a new rung, a changed
// threshold, a different sanitization policy, an upgraded extractor library.
// `tome reextract --since-version <n>` uses it to find everything produced by
// older behavior. Forgetting to bump it means an improvement silently never
// reaches the archive it was written for.
// Version 2 (2026-08-17): the ratio check is skipped for bodies past longBody.
// See the constant for the measurement that prompted it.
const Version = "2"

// Extractor names recorded on content rows.
const (
	NameDomainRule  = "domain_rule"
	NameTrafilatura = "trafilatura"
	NameReadability = "readability"
	NameFeedBody    = "feed_body"
)

// Acceptance thresholds (§5.4).
const (
	// minChars is the shortest body worth calling an article. Below this it is
	// almost always a paywall stub, a cookie notice, or a navigation shell.
	minChars = 200

	// minRatio is the share of the page's visible text the body must account
	// for. It catches the opposite failure: an extractor that returns the
	// whole page including navigation looks successful by length alone.
	minRatio = 0.25

	// longBody is the length past which the ratio check no longer applies.
	//
	// The ratio exists to catch a navigation fragment mistaken for the article,
	// and a fragment is short. Past this length the text is prose, and demanding
	// that it also be a fixed share of the page punishes sites whose chrome is
	// simply large.
	//
	// Measured against the real feed list on 2026-08-17: on a Hugo/Docsy
	// documentation theme the sidebar alone is roughly 40,000 characters of
	// visible text, so whole-page visible length sat at 45,000–54,000 for every
	// article while the posts themselves ran 3,500–13,300. Every one scored under
	// 0.25 and was rejected — the ratio was measuring post length against sidebar
	// size, which is not a property of the extraction at all. 42 of 50 articles
	// fell through to the feed body as a result.
	//
	// The asymmetry that sets the value: a false reject stores a truncated feed
	// summary while the full page sits on disk unread, which is precisely the
	// failure §1 exists to prevent. A false accept stores a short-but-real body
	// that §2.2 lets us re-extract later. So the absolute floor wins, and it is
	// set low enough to clear the whole measured range.
	longBody = 2000
)

// Rule is a per-domain override from the domain_rules table.
type Rule struct {
	ContentSelector string
	StripSelectors  []string
}

// Result is one extracted body, ready to be written as a content row.
type Result struct {
	// Name is which rung produced this, recorded as extractor_name.
	Name string

	// HTML is the sanitized body. Image sources are absolute; M3 rewrites
	// them to relative blob paths once the images are localized.
	HTML string

	// Text is the plain text, used for search and for the length checks.
	Text string

	WordCount int

	// Metadata the extractor recovered from the page. Each field is empty when
	// the page did not carry it; the caller fills gaps rather than overwriting
	// what a feed already supplied.
	Title       string
	Author      string
	SiteName    string
	Language    string
	PublishedAt *time.Time
}

// Extractor runs the ladder. It is safe for concurrent use.
type Extractor struct {
	sanitizer *sanitizer
}

// New returns an Extractor.
func New() *Extractor {
	return &Extractor{sanitizer: newSanitizer()}
}

// Input is everything a single extraction has to work with.
type Input struct {
	// RawHTML is the stored page. Empty when the fetch failed, in which case
	// only the feed-body rung can produce anything.
	RawHTML []byte

	// URL is the article's canonical URL, used to resolve relative links and
	// as context for the extractors.
	URL string

	// Rule is the domain override for this host, or nil.
	Rule *Rule

	// FeedBody is the content the feed itself carried, used by the last rung.
	FeedBody string
}

// ErrNoContent means every rung failed to produce an acceptable body.
//
// This is an expected outcome, not a bug: some pages are paywalled, some are
// JavaScript shells, some are 404s that returned 200. The caller records it on
// the article so the failure is visible in the queue that domain rules exist
// to drain, rather than being retried forever.
var ErrNoContent = fmt.Errorf("no extractor produced acceptable content")

// Extract runs the ladder and returns the first acceptable result.
func (e *Extractor) Extract(in Input) (Result, error) {
	pageURL, err := url.Parse(in.URL)
	if err != nil {
		return Result{}, fmt.Errorf("parsing article URL %q: %w", in.URL, err)
	}

	// The denominator for the ratio check, computed once. A page whose visible
	// text cannot be measured falls back to the length check alone.
	visible := visibleTextLength(in.RawHTML)

	if len(in.RawHTML) > 0 {
		// Rung 1. A domain rule is a human saying "the body is here", so it
		// overrides the ratio check: the whole reason a rule exists is that
		// the heuristics were wrong about this site.
		if in.Rule != nil && in.Rule.ContentSelector != "" {
			if r, ok := e.viaDomainRule(in, pageURL); ok {
				return r, nil
			}
		}

		// Rung 2.
		if r, ok := e.viaTrafilatura(in, pageURL, visible); ok {
			return r, nil
		}

		// Rung 3.
		if r, ok := e.viaReadability(in, pageURL, visible); ok {
			return r, nil
		}
	}

	// Rung 5. The feed body is not compared against the page's text — it is a
	// different document, and comparing them would reject every truncated
	// summary for being short relative to a page it does not come from.
	if r, ok := e.viaFeedBody(in, pageURL); ok {
		return r, nil
	}

	return Result{}, ErrNoContent
}

func (e *Extractor) viaDomainRule(in Input, pageURL *url.URL) (Result, bool) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(in.RawHTML))
	if err != nil {
		return Result{}, false
	}

	for _, sel := range in.Rule.StripSelectors {
		if strings.TrimSpace(sel) != "" {
			doc.Find(sel).Remove()
		}
	}

	selection := doc.Find(in.Rule.ContentSelector)
	if selection.Length() == 0 {
		// The rule no longer matches — the site was redesigned. Falling
		// through to the heuristics is better than returning nothing, and the
		// recorded extractor name will show the rule was not what ran.
		return Result{}, false
	}

	body, err := selection.Html()
	if err != nil {
		return Result{}, false
	}

	text := strings.TrimSpace(selection.Text())
	if len(text) < minChars {
		return Result{}, false
	}

	return e.finish(NameDomainRule, body, text, pageURL, docMetadata(doc)), true
}

func (e *Extractor) viaTrafilatura(in Input, pageURL *url.URL, visible int) (Result, bool) {
	// EnableFallback is off because this ladder runs readability itself as the
	// next rung. Letting trafilatura fall back internally would produce a
	// readability result labeled "trafilatura", and the labels are what make
	// extraction quality measurable per extractor.
	result, err := trafilatura.Extract(bytes.NewReader(in.RawHTML), trafilatura.Options{
		OriginalURL:     pageURL,
		IncludeImages:   true,
		IncludeLinks:    true,
		ExcludeComments: true,
		Deduplicate:     true,
		EnableFallback:  false,
	})
	if err != nil || result == nil || result.ContentNode == nil {
		return Result{}, false
	}

	text := strings.TrimSpace(result.ContentText)
	if !acceptable(text, visible) {
		return Result{}, false
	}

	body, err := renderNode(result.ContentNode)
	if err != nil {
		return Result{}, false
	}

	meta := metadata{
		Title:    result.Metadata.Title,
		Author:   result.Metadata.Author,
		SiteName: result.Metadata.Sitename,
		Language: result.Metadata.Language,
	}
	if !result.Metadata.Date.IsZero() {
		d := result.Metadata.Date
		meta.PublishedAt = &d
	}

	return e.finish(NameTrafilatura, body, text, pageURL, meta), true
}

func (e *Extractor) viaReadability(in Input, pageURL *url.URL, visible int) (Result, bool) {
	article, err := readability.FromReader(bytes.NewReader(in.RawHTML), pageURL)
	if err != nil {
		return Result{}, false
	}

	text := strings.TrimSpace(article.TextContent)
	if !acceptable(text, visible) {
		return Result{}, false
	}

	return e.finish(NameReadability, article.Content, text, pageURL, metadata{
		Title:       article.Title,
		Author:      article.Byline,
		SiteName:    article.SiteName,
		Language:    article.Language,
		PublishedAt: article.PublishedTime,
	}), true
}

func (e *Extractor) viaFeedBody(in Input, pageURL *url.URL) (Result, bool) {
	body := strings.TrimSpace(in.FeedBody)
	if body == "" {
		return Result{}, false
	}

	text := strings.TrimSpace(textOf([]byte(body)))
	// No ratio check, but still a floor: a two-sentence summary is not an
	// article, and storing it as one would make the archive look complete
	// when it is not.
	if len(text) < minChars {
		return Result{}, false
	}

	return e.finish(NameFeedBody, body, text, pageURL, metadata{}), true
}

// metadata is what a rung recovered about the article itself.
type metadata struct {
	Title       string
	Author      string
	SiteName    string
	Language    string
	PublishedAt *time.Time
}

// finish applies the steps every rung shares: resolve relative URLs against
// the article, sanitize, and count words.
//
// Resolution happens before sanitization on purpose. The sanitizer's URL
// policy accepts absolute http and https; resolving first means a relative
// image survives instead of being stripped as an unrecognized reference.
func (e *Extractor) finish(name, body, text string, pageURL *url.URL, meta metadata) Result {
	resolved := resolveURLs(body, pageURL)
	clean := e.sanitizer.sanitize(resolved)

	return Result{
		Name:        name,
		HTML:        clean,
		Text:        text,
		WordCount:   len(strings.Fields(text)),
		Title:       strings.TrimSpace(meta.Title),
		Author:      strings.TrimSpace(meta.Author),
		SiteName:    strings.TrimSpace(meta.SiteName),
		Language:    strings.TrimSpace(meta.Language),
		PublishedAt: meta.PublishedAt,
	}
}

// acceptable implements the §5.4 threshold: long enough in absolute terms, and
// — for bodies short enough that the question is open — a large enough share of
// the page to be the article rather than the chrome.
func acceptable(text string, visibleLen int) bool {
	if len(text) < minChars {
		return false
	}
	if len(text) >= longBody {
		// Long enough to be prose on its own terms. See longBody: on a
		// chrome-heavy site the ratio measures the page's furniture, not the
		// quality of the extraction.
		return true
	}
	if visibleLen <= 0 {
		// The page's visible text could not be measured, so the ratio is
		// meaningless. The length check alone still applies.
		return true
	}
	return float64(len(text)) >= minRatio*float64(visibleLen)
}

// visibleTextLength measures the text a reader would see on the whole page,
// which is the denominator of the ratio check.
func visibleTextLength(raw []byte) int {
	return len(strings.TrimSpace(textOf(raw)))
}

// textOf returns the visible text of an HTML fragment or document.
func textOf(raw []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	// Script and style contents are text nodes in the parse tree but are not
	// visible; counting them would inflate the denominator and reject good
	// extractions from script-heavy pages.
	doc.Find("script, style, noscript, template").Remove()
	return doc.Text()
}

// renderNode serializes an html.Node back to markup.
func renderNode(node *html.Node) (string, error) {
	var buf bytes.Buffer
	if err := html.Render(&buf, node); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// docMetadata pulls the standard metadata tags from a parsed document, for the
// domain-rule rung, which has no extractor library to do it.
func docMetadata(doc *goquery.Document) metadata {
	attr := func(selector, name string) string {
		v, _ := doc.Find(selector).First().Attr(name)
		return strings.TrimSpace(v)
	}

	m := metadata{
		Title:    attr(`meta[property="og:title"]`, "content"),
		Author:   attr(`meta[name="author"]`, "content"),
		SiteName: attr(`meta[property="og:site_name"]`, "content"),
		Language: attr("html", "lang"),
	}
	if m.Title == "" {
		m.Title = strings.TrimSpace(doc.Find("title").First().Text())
	}
	return m
}
