package extract

import (
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/microcosm-cc/bluemonday"
)

// sanitizer strips anything from an extracted body that could execute, track,
// or reach back out to the network.
//
// This matters more here than in most applications. The archive renders HTML
// that arbitrary websites authored, in the reader's browser, on the reader's
// own origin, and it keeps doing so for a decade. A script that survives
// extraction runs with the session cookie of whoever opens the article. So the
// policy is an allowlist, and it is deliberately narrow.
type sanitizer struct {
	policy *bluemonday.Policy
}

func newSanitizer() *sanitizer {
	p := bluemonday.UGCPolicy()

	// §5.4: the UGC policy plus modern figure and responsive-image markup,
	// without which every article loses its captions and its images.
	p.AllowElements("figure", "figcaption", "picture")
	p.AllowAttrs("srcset", "sizes", "media", "type").OnElements("source")
	p.AllowElements("source")
	p.AllowAttrs("srcset", "sizes", "loading", "width", "height").OnElements("img")

	// Semantic elements that carry an article's structure. Losing these turns
	// a well-marked-up piece into undifferentiated paragraphs.
	p.AllowElements("article", "section", "header", "footer", "aside", "main",
		"time", "mark", "small", "cite", "q", "dfn", "abbr", "sub", "sup",
		"figcaption", "hgroup")
	p.AllowAttrs("datetime").OnElements("time")
	p.AllowAttrs("cite").OnElements("blockquote", "q")

	// Code blocks keep their language hint, which is what makes syntax
	// highlighting possible later without re-guessing.
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "pre", "span")

	// Only http and https. This is what rejects javascript:, data:, and
	// file: URLs, each of which is a way for archived markup to do something
	// other than be read.
	p.AllowURLSchemes("http", "https")

	// Relative URLs are rejected because finish() has already resolved every
	// reference against the article's own URL. Anything still relative at this
	// point is malformed, and a relative link in the archive would resolve
	// against whatever page happens to display it.
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)

	// Outbound links get rel="nofollow noreferrer" and open in a new tab: the
	// archive should not leak the reader's location back to sites it links to,
	// nor pass on ranking signal to whatever a decade-old page pointed at.
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	return &sanitizer{policy: p}
}

func (s *sanitizer) sanitize(body string) string {
	return strings.TrimSpace(s.policy.Sanitize(body))
}

// resolveURLs rewrites every relative reference in a body to an absolute URL
// against the article's own address.
//
// Two reasons this has to happen. The archive is served from a different path
// than the origin, so a relative link would resolve to the wrong place; and
// M3's asset pipeline needs absolute image URLs to fetch, since it works from
// the stored body rather than from the live page.
func resolveURLs(body string, base *url.URL) string {
	if base == nil {
		return body
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return body
	}

	resolve := func(raw string) (string, bool) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "data:") {
			// Data URIs are already self-contained; §5.5 keeps them as they
			// are rather than treating them as fetchable assets.
			return "", false
		}
		ref, err := url.Parse(raw)
		if err != nil {
			return "", false
		}
		return base.ResolveReference(ref).String(), true
	}

	for _, sel := range []struct{ selector, attr string }{
		{"a[href]", "href"},
		{"img[src]", "src"},
		{"source[src]", "src"},
		{"video[src]", "src"},
		{"audio[src]", "src"},
	} {
		doc.Find(sel.selector).Each(func(_ int, node *goquery.Selection) {
			if raw, ok := node.Attr(sel.attr); ok {
				if abs, ok := resolve(raw); ok {
					node.SetAttr(sel.attr, abs)
				}
			}
		})
	}

	// srcset is a comma-separated list of "url descriptor" pairs, so each
	// candidate has to be resolved individually. Getting this wrong loses
	// every responsive image on sites that only use srcset.
	doc.Find("[srcset]").Each(func(_ int, node *goquery.Selection) {
		raw, ok := node.Attr("srcset")
		if !ok {
			return
		}
		node.SetAttr("srcset", resolveSrcset(raw, resolve))
	})

	// goquery wraps fragments in html/body; unwrap to keep the body a
	// fragment rather than a nested document.
	out, err := doc.Find("body").Html()
	if err != nil {
		return body
	}
	return out
}

func resolveSrcset(raw string, resolve func(string) (string, bool)) string {
	candidates := strings.Split(raw, ",")
	out := make([]string, 0, len(candidates))

	for _, candidate := range candidates {
		fields := strings.Fields(strings.TrimSpace(candidate))
		if len(fields) == 0 {
			continue
		}
		abs, ok := resolve(fields[0])
		if !ok {
			out = append(out, strings.TrimSpace(candidate))
			continue
		}
		fields[0] = abs
		out = append(out, strings.Join(fields, " "))
	}
	return strings.Join(out, ", ")
}
