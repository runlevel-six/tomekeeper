// Package extract turns a fetched HTML page into a readable article body.
//
// Extraction is a *derived, versioned view* over the stored raw fetch, never
// the authoritative copy. Extraction quality only improves, so
// every body carries the name and version of what produced it and can be
// regenerated in bulk by `tome reextract` without re-fetching anything.
//
// The ladder runs in order and stops at the first acceptable result:
//
//  1. A domain rule's CSS selector, when one exists for the host.
//  2. go-trafilatura, the primary extractor.
//  3. go-readability, the fallback.
//  4. Headless rendering — planned, for domains flagged requires_js.
//  5. The feed's own body, when everything else failed.
//
// Rung 4 is deliberately absent here: nothing in this package starts a
// browser. When that lands it becomes another Extractor implementation.
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
// `tome reextract --target-version <n>` uses it to find everything produced by
// older behavior. Forgetting to bump it means an improvement silently never
// reaches the archive it was written for.
// Version 2 (2026-08-17): the ratio check is skipped for bodies past longBody.
// See the constant for the measurement that prompted it.
//
// Version 4 (2026-08-18): two changes to what a body contains, adopted together so
// that one re-extraction pays for both.
//
//   - Inline images survive. The sanitizer stripped every data: URI, including the
//     image ones, while three other parts of the archive assumed they did not.
//   - Text has a boundary where the markup has one. Block edges produced
//     "service.Data" out of `<p>service.</p><h2>Data center</h2>`, which the length
//     checks measured, the search index tokenized, and an excerpt showed to a
//     reader. 16 of 341 bodies in a real archive carried at least one.
//
// Version 5 (2026-08-19): the page images rung finds a strip named after the page's
// title as well as after its URL, and a thin body loses to those images unless it
// already holds one of them. Every webcomic whose article URLs are bare numbers was
// stored as its own footer until this — see orThePageImagesIfTextless.
const Version = "5"

// Extractor names recorded on content rows.
const (
	NameDomainRule  = "domain_rule"
	NameTrafilatura = "trafilatura"
	NameReadability = "readability"
	NameFeedBody    = "feed_body"
	NamePageImages  = "page_images"

	// NameImported marks a body that arrived already extracted from another
	// system. No rung produced it, and none should ever replace it.
	NameImported = "imported"
)

// Acceptance thresholds.
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
	// failure this whole project exists to prevent. A false accept stores a short-but-real body
	// that the store-the-raw-fetch principle lets us re-extract later. So the absolute floor wins, and it is
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

	// HTML is the sanitized body. Image sources are absolute; the asset pipeline
	// rewrites
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

// Step is what one rung of the ladder did, for explaining an extraction after the
// fact.
//
// An extraction that fails reports one sentence — "no extractor produced acceptable
// content" — which is true and useless. The maintenance loop this archive is built
// around goes from the attention queue to a domain rule, and the missing step in
// the middle is *why*: which rungs ran, what each produced, and which threshold
// turned it down. Without that, a page that stops extracting is a dig site.
type Step struct {
	// Rung is the extractor name, or "page" for the measurement of the whole
	// document that the ratio check compares against.
	Rung string

	// Ran is false for a rung that was skipped entirely — a domain rule with no
	// selector, or anything needing a stored page when there is none.
	Ran bool

	// Chars, Words and Images describe what the rung produced. Zero when it
	// produced nothing.
	Chars  int
	Words  int
	Images int

	// Accepted says whether the ladder took this result.
	Accepted bool

	// Why explains the decision in the terms the thresholds are written in.
	Why string
}

// Extract runs the ladder and returns the first acceptable result.
func (e *Extractor) Extract(in Input) (Result, error) {
	result, _, err := e.run(in, false)
	return result, err
}

// Explain runs the ladder and reports what every rung did.
//
// Same code path as Extract, deliberately: an explanation produced by a second
// implementation of the ladder would be a description of a program that does not
// exist, and it would drift the first time a rung changed.
func (e *Extractor) Explain(in Input) (Result, []Step, error) {
	return e.run(in, true)
}

// run is the ladder. explain costs an extra measurement per rung and is off for the
// path that runs on every article.
func (e *Extractor) run(in Input, explain bool) (Result, []Step, error) {
	pageURL, err := url.Parse(in.URL)
	if err != nil {
		return Result{}, nil, fmt.Errorf("parsing article URL %q: %w", in.URL, err)
	}

	// The denominator for the ratio check, computed once. A page whose visible
	// text cannot be measured falls back to the length check alone.
	visible := visibleTextLength(in.RawHTML)

	var steps []Step
	record := func(s Step) {
		if explain {
			steps = append(steps, s)
		}
	}

	if len(in.RawHTML) == 0 {
		record(Step{Rung: "page", Why: "no stored page, so only the feed body can produce anything"})
	} else {
		record(Step{
			Rung: "page", Ran: true, Chars: visible,
			Why: fmt.Sprintf("%d characters of visible text; a body under %d characters must be "+
				"at least %.0f%% of it (%d characters)",
				visible, longBody, minRatio*100, int(minRatio*float64(visible))),
		})
	}

	if len(in.RawHTML) > 0 {
		// Rung 1. A domain rule is a human saying "the body is here", so it
		// overrides the ratio check: the whole reason a rule exists is that
		// the heuristics were wrong about this site.
		if in.Rule != nil && in.Rule.ContentSelector != "" {
			r, ok := e.viaDomainRule(in, pageURL)
			record(describe(NameDomainRule, r, ok, ruleWhy(in, r, ok)))
			if ok {
				return r, steps, nil
			}
		} else {
			record(Step{Rung: NameDomainRule, Why: "no rule for this domain"})
		}

		// Rung 2.
		r, ok := e.viaTrafilatura(in, pageURL, visible)
		record(describe(NameTrafilatura, r, ok, acceptWhy(r, ok, visible)))
		if ok {
			return e.bestOf(in, pageURL, r), steps, nil
		}

		// Rung 3.
		r, ok = e.viaReadability(in, pageURL, visible)
		record(describe(NameReadability, r, ok, acceptWhy(r, ok, visible)))
		if ok {
			return e.bestOf(in, pageURL, r), steps, nil
		}
	}

	// Rung 4. The feed body is not compared against the page's text — it is a
	// different document, and comparing them would reject every truncated
	// summary for being short relative to a page it does not come from.
	if in.FeedBody == "" {
		record(Step{Rung: NameFeedBody, Why: "the feed carried no body for this article"})
	} else {
		r, ok := e.viaFeedBody(in, pageURL)
		record(describe(NameFeedBody, r, ok, feedWhy(r, ok)))
		if ok {
			return r, steps, nil
		}
	}

	// Rung 5. The page's own images, for articles that are a picture.
	//
	// Last deliberately, and after the feed body rather than before it. A page
	// whose text extraction failed is usually paywalled or JavaScript-rendered,
	// not a comic, and for those the feed's words are worth more than the
	// article's hero image. Ordering this rung first would trade real prose for
	// a picture on every one of them.
	if len(in.RawHTML) > 0 {
		r, ok := e.viaPageImages(in, pageURL)
		record(describe(NamePageImages, r, ok, imagesWhy(in, r, ok)))
		if ok {
			return r, steps, nil
		}
	}

	return Result{}, steps, ErrNoContent
}

// describe turns a rung's outcome into a Step.
func describe(name string, r Result, ok bool, why string) Step {
	return Step{
		Rung:     name,
		Ran:      true,
		Chars:    len(r.Text),
		Words:    r.WordCount,
		Images:   countImages(r.HTML),
		Accepted: ok,
		Why:      why,
	}
}

// acceptWhy states the threshold that decided a heuristic rung, in its own terms.
func acceptWhy(r Result, ok bool, visible int) string {
	switch {
	case ok && len(r.Text) >= longBody:
		return fmt.Sprintf("%d characters, past the %d the ratio check stops applying at",
			len(r.Text), longBody)
	case ok:
		return fmt.Sprintf("%d characters, over the %d floor and %.0f%% of the page's visible text",
			len(r.Text), minChars, share(len(r.Text), visible))
	case len(r.Text) == 0:
		return "produced nothing"
	case len(r.Text) < minChars:
		return fmt.Sprintf("%d characters, under the %d-character floor", len(r.Text), minChars)
	default:
		return fmt.Sprintf("%d characters, only %.0f%% of the page's %d — under the %.0f%% "+
			"a body this short has to reach",
			len(r.Text), share(len(r.Text), visible), visible, minRatio*100)
	}
}

// ruleWhy reports a rule's outcome, and prints its selector unquoted.
//
// %q would escape the quotes inside an attribute selector, so what an operator
// reads back is not what they would paste into the rule — and the thing they are
// most likely to do with this line is exactly that.
func ruleWhy(in Input, r Result, ok bool) string {
	if ok {
		return fmt.Sprintf("the rule's selector (%s) matched, and a rule overrides the ratio check",
			in.Rule.ContentSelector)
	}
	if len(r.Text) == 0 {
		return fmt.Sprintf("the rule's selector (%s) matched nothing on this page",
			in.Rule.ContentSelector)
	}
	return fmt.Sprintf("the rule matched %d characters, under the %d-character floor, and the "+
		"selection carries no image", len(r.Text), minChars)
}

func feedWhy(r Result, ok bool) string {
	if ok {
		return fmt.Sprintf("the feed's own body, %d characters", len(r.Text))
	}
	return fmt.Sprintf("the feed's body is %d characters, under the %d-character floor",
		len(r.Text), minChars)
}

func imagesWhy(in Input, r Result, ok bool) string {
	if ok {
		return fmt.Sprintf("%d image(s) named after this article's slug or its title",
			countImages(r.HTML))
	}
	return "no image on the page is named after this article's slug or its title, so none of " +
		"them is its content"
}

// share is a percentage that does not divide by zero.
func share(part, whole int) float64 {
	if whole <= 0 {
		return 100
	}
	return float64(part) / float64(whole) * 100
}

// countImages counts the images in a body, for explaining a rung's output.
func countImages(body string) int { return strings.Count(body, "<img") }

// CleanImported prepares a body that arrived already extracted from another
// system.
//
// Not a rung and not part of the ladder: there is nothing to choose between and
// no threshold to clear. What it does is run an imported body through exactly the
// steps every rung's output goes through — resolve references against the
// article's own URL, then sanitize — because the archive renders stored bodies as
// trusted HTML on the reader's own origin, and it does so for a decade.
//
// Going through the same policy rather than a second one built for importers is
// the whole point. A body from someone else's reader is markup an arbitrary
// website authored, no different in kind from a page this archive fetched itself,
// and it is *older*: a decade-old save can carry script that predates every
// mitigation. An importer with its own allowlist would be a second policy to keep
// in step with this one, and the failure mode of them drifting apart is a stored
// script running with the session cookie of whoever opens the article.
//
// Rejects nothing. Length thresholds exist to choose between rungs, and a caller
// with one already-extracted body has no choice to make — deciding whether the
// body is worth having at all is the importer's job, and it is one an importer can
// do better, because it knows what its source puts in the field when a fetch
// failed.
func (e *Extractor) CleanImported(body, articleURL string) Result {
	// A URL that will not parse costs the body its relative references, not the
	// import. An imported article's URL comes from another system's database and
	// may be a decade of drift away from anything parseable.
	pageURL, err := url.Parse(articleURL)
	if err != nil {
		pageURL = nil
	}

	text := strings.TrimSpace(textOf([]byte(body)))
	return e.finish(NameImported, body, text, pageURL, metadata{})
}

// feedAdvantage is how many times richer the feed body must be before it is
// preferred over a body extracted from the page.
//
// Three, which is a wide margin, because this rule runs against a rung that
// already declared success and the cost of being wrong is storing a summary in
// place of a real article — the failure the whole extraction ladder exists to
// avoid.
const feedAdvantage = 3

// orTheFeedIfRicher returns the feed body instead when the page extraction is
// implausibly thin beside it.
//
// The ladder otherwise takes the first rung that succeeds, and "success" is a
// floor rather than a judgment: an extractor that returns a page's header block
// clears it. Observed on a real feed, where trafilatura returned 30 words —
// the title, twice, and two dates — while the feed carried the entire 2,000-word
// article that the ladder then threw away.
//
// The comparison is sound in one direction only, which is what makes it safe. A
// feed summary is a truncation of the article, so it cannot legitimately be
// several times longer than the article's own body; when it is, the extraction
// missed the content rather than the feed having gained any. That is why this
// cannot cause the failure §1 warns about — storing a truncated summary while
// the full page sits on disk — because it only ever moves toward the *longer*
// text, never the shorter.
func (e *Extractor) orTheFeedIfRicher(in Input, pageURL *url.URL, page Result) Result {
	if strings.TrimSpace(in.FeedBody) == "" {
		return page
	}

	feed, ok := e.viaFeedBody(in, pageURL)
	if !ok {
		return page
	}
	if feed.WordCount < page.WordCount*feedAdvantage {
		return page
	}
	return feed
}

// bestOf second-guesses a rung that declared success.
//
// Both corrections it applies exist because "acceptable" is a floor rather than
// a judgment, and a page's header block or its navigation sidebar clears a
// floor as easily as an article does.
func (e *Extractor) bestOf(in Input, pageURL *url.URL, page Result) Result {
	page = e.orTheFeedIfRicher(in, pageURL, page)
	return e.orThePageImagesIfTextless(in, pageURL, page)
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

	body, err := joinHTML(selection)
	if err != nil {
		return Result{}, false
	}

	text := strings.TrimSpace(selection.Text())

	// The length floor does not apply to a selection carrying an image.
	//
	// A rule is a human saying "the body is here", and on an image-first page
	// the body is a picture and a caption — nowhere near 200 characters. Judging
	// it by text length rejects the one thing the operator explicitly pointed
	// at, which is how a webcomic could be neither extracted automatically nor
	// rescued by hand.
	//
	// Safe because a rule is not a heuristic: nothing selects this element
	// except someone who chose it.
	if len(text) < minChars && !hasImage(body) {
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
		// The measured text comes back even though this rung is refusing it, so that
		// the explanation can say how short it actually was. Returning a zeroed Result
		// here made every rejection report "0 characters" — indistinguishable between a
		// feed body of pure markup and one that missed the floor by a single character,
		// and it sent somebody looking for a data-loss bug that did not exist. The false
		// is what stops it being used; the number is only ever read for the report.
		return Result{Text: text}, false
	}

	return e.finish(NameFeedBody, body, text, pageURL, metadata{}), true
}

// joinHTML concatenates the contents of every element a rule selected.
//
// Every element, which sounds obvious and was not: goquery's Html() returns the
// contents of the *first* matched element only, while Text() returns the text of
// them all. A selector matching three blocks therefore produced a body holding one
// of them and a text holding three — and because the ladder's length checks read
// the text, the result passed every threshold and looked like a working rule. What
// the reader got was the first third of the article; what search indexed was all of
// it. The two are stored in the same row and are meant to be the same document.
//
// This is not a rare shape. A site that breaks its article around mid-article
// advertising emits exactly this: several sibling content blocks, each complete,
// with the furniture between them. Ars Technica is one, and the truncation is
// invisible from the outside because the first block is a plausible article.
//
// Elements nested inside another match are skipped. A rule naming both a wrapper
// and something inside it — `.lightbox, .post-content`, where a lightbox also
// appears within a post-content block — would otherwise emit that inner element
// twice, and a duplicated image in the middle of an article is a strange thing to
// have to explain. Taking the outermost is what somebody writing that selector
// means.
func joinHTML(selection *goquery.Selection) (string, error) {
	matched := make(map[*html.Node]bool, selection.Length())
	selection.Each(func(_ int, s *goquery.Selection) {
		for _, node := range s.Nodes {
			matched[node] = true
		}
	})

	var (
		joined strings.Builder
		failed error
	)
	selection.EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if nestedIn(matched, s.Nodes[0]) {
			return true
		}
		inner, err := s.Html()
		if err != nil {
			failed = err
			return false
		}
		joined.WriteString(inner)
		return true
	})
	if failed != nil {
		return "", failed
	}
	return joined.String(), nil
}

// nestedIn reports whether a node has an ancestor that was also selected.
func nestedIn(matched map[*html.Node]bool, node *html.Node) bool {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if matched[parent] {
			return true
		}
	}
	return false
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

// acceptable implements the ladder threshold: long enough in absolute terms, and
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

// blockElements are the tags a reader sees a boundary at.
//
// Not an exhaustive list of block-level HTML, and not meant to be: what matters is
// the elements that actually separate words in real markup. `<br>` is here because
// it is a line break; `<td>` because a table row read without separators becomes one
// long word.
var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true,
	"dd": true, "div": true, "dl": true, "dt": true, "figcaption": true,
	"figure": true, "footer": true, "form": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "header": true, "hr": true,
	"li": true, "main": true, "nav": true, "ol": true, "p": true, "pre": true,
	"section": true, "table": true, "td": true, "th": true, "tr": true, "ul": true,
}

// textOf returns the visible text of an HTML fragment or document, with a boundary
// where the markup has one.
//
// goquery's Text() concatenates text nodes with nothing between them, so
// `<p>service.</p><h2>Data center</h2>` comes out as "service.Data center" — two
// words welded into one. That is wrong in three places at once: the length checks
// measure it, the search index tokenizes it, and an excerpt shows it to a reader.
//
// Measured on a real 385-article archive while building the export: 16 of 341 bodies
// carried at least one of these, 46 words in total. Small, and the kind of small
// that is invisible until somebody searches for a word that got welded to the one
// before it.
func textOf(raw []byte) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	// Script and style contents are text nodes in the parse tree but are not
	// visible; counting them would inflate the denominator and reject good
	// extractions from script-heavy pages.
	doc.Find("script, style, noscript, template").Remove()

	var out textWriter
	for _, node := range doc.Nodes {
		out.write(node)
	}
	return out.b.String()
}

// textWriter accumulates visible text, collapsing the boundaries it inserts.
//
// It tracks whether the last thing written was whitespace rather than asking the
// builder, because asking means copying the whole string on every element of every
// page.
type textWriter struct {
	b       strings.Builder
	pending bool // a boundary is owed before the next text
	written bool
}

func (w *textWriter) write(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.ElementNode:
		if blockElements[n.Data] {
			w.boundary()
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.write(c)
	}

	if n.Type == html.ElementNode && blockElements[n.Data] {
		w.boundary()
	}
}

// text writes a text node, honoring any boundary owed from an element edge.
func (w *textWriter) text(s string) {
	if s == "" {
		return
	}
	if w.pending && w.written && !startsWithSpace(s) {
		w.b.WriteByte('\n')
	}
	w.pending = false
	w.b.WriteString(s)
	w.written = true
}

// boundary records that the next text is separated from what came before. Recorded
// rather than written, so that a run of nested block elements — a div inside a
// section inside an article — produces one separator rather than three.
func (w *textWriter) boundary() { w.pending = true }

func startsWithSpace(s string) bool {
	switch s[0] {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
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
