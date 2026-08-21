package server_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The size has to be in the markup the browser lays out first, not applied to it
// afterwards.
//
// This is the whole reason it is a column on users rather than a cookie a script
// reads: a palette applied late flashes the wrong colors, but a size applied late
// reflows the entire page under someone who has already started reading it. So the
// assertion is about *where* the value appears — on the document element, in the
// first bytes — and not merely that it round-trips.
func TestTheTextSizeIsInTheFirstPaint(t *testing.T) {
	rd, _ := readingFixture(t)

	if rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {"auto"}, "mode": {"auto"},
		"text_size":  {store.TextScaleLarger},
		"poll_every": {""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d, want 200", rec.Code)
	}

	body := rd.body("/")

	// On <html>, which is the element the stylesheet maps and the first one the
	// parser sees.
	head := body
	if i := strings.Index(body, "<head>"); i > 0 {
		head = body[:i]
	}
	if !strings.Contains(head, `data-text="larger"`) {
		t.Errorf("the chosen size is not on the document element:\n%s", head)
	}

	// And nowhere in a style attribute, which style-src 'self' would refuse.
	if strings.Contains(body, `style="--text-scale`) || strings.Contains(body, "style=\"--text") {
		t.Error("the size traveled as an inline style, which the content security policy blocks")
	}
}

// Every page, not only the one the setting was saved from: the size is chrome, and
// a reader who set it once should not find it applying to some views and not others.
func TestTheTextSizeIsOnEveryView(t *testing.T) {
	rd, tr := readingFixture(t)
	seedManyUnread(t, tr, 2)

	if rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {"auto"}, "mode": {"auto"},
		"text_size":  {store.TextScaleLargest},
		"poll_every": {""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d, want 200", rec.Code)
	}

	for _, path := range []string{"/", "/all", "/starred", "/saved", "/feeds", "/categories", "/settings", "/attention"} {
		if !strings.Contains(rd.body(path), `data-text="largest"`) {
			t.Errorf("%s does not carry the reader's text size", path)
		}
	}
}

// The default has to be absent rather than spelled out, exactly as 'auto' is for
// the palette: an attribute for the default value is a second way to say the same
// thing, and the stylesheet would then need a rule that does nothing.
func TestTheDefaultTextSizeIsNotWrittenOut(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/")
	if strings.Contains(body, "data-text=") {
		t.Errorf("a reader who has chosen nothing still got a size attribute:\n%s",
			body[:min(len(body), 300)])
	}
}

// A value the stylesheet cannot map is stored as the default rather than as
// itself, so what is shown and what is stored cannot disagree.
func TestAnUnknownTextSizeBecomesTheDefault(t *testing.T) {
	rd, _ := readingFixture(t)

	if rec := rd.do(http.MethodPost, "/settings", url.Values{
		"palette": {"auto"}, "mode": {"auto"},
		"text_size":  {"enormous'; drop table users; --"},
		"poll_every": {""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("POST /settings = %d, want 200", rec.Code)
	}

	body := rd.body("/")
	if strings.Contains(body, "data-text=") {
		t.Errorf("an unrecognized size reached the document element:\n%s",
			body[:min(len(body), 300)])
	}
	if strings.Contains(rd.body("/settings"), "enormous") {
		t.Error("an unrecognized size was stored and echoed back into the form")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The article's supporting text is derived from its body rather than pinned, and
// this is what keeps it that way.
//
// The stylesheet is asserted directly because there is nothing else to assert
// against: no browser here computes it, and the failure mode is silent. It used to
// have four supporting sizes — the byline at 12.8px, the outbound link at 13.6, the
// image notice at 13.6 and captions at 14.2 — which read as one size with rounding
// errors, and none of which moved when the body did. A size preference over that
// would have grown the body away from its own captions.
func TestTheArticleHierarchyIsDerivedFromOneSize(t *testing.T) {
	css := readStylesheet(t)

	// One declaration of the body size, and the supporting tier computed from it.
	for _, want := range []string{
		"--reader-body: clamp(",
		"--reader-support: calc(var(--reader-body)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the reader's type scale does not declare %q", want)
		}
	}

	// The four that used to each pick their own near-identical size now name the
	// tier. A pinned rem here is the bug this replaced.
	for _, decl := range []string{
		".reader header .meta { font-size: var(--reader-support); }",
		".reader .image-notice { font-size: var(--reader-support);",
	} {
		if !strings.Contains(css, decl) {
			t.Errorf("expected the supporting tier to be used by:\n  %s", decl)
		}
	}

	// And the body no longer repeats the clamp, which is how the two drifted apart.
	if strings.Count(css, "clamp(1.0625rem") != 1 {
		t.Errorf("the body size is declared %d times; it should exist once, as --reader-body",
			strings.Count(css, "clamp(1.0625rem"))
	}
}

// Every size in the stylesheet has to be relative for the preference to reach it.
// A px font-size is a size the reader cannot change, and the one that was there
// overrode the browser's own setting for everything inheriting from body.
func TestNoStylesheetSizeIsPinnedInPixels(t *testing.T) {
	css := readStylesheet(t)

	// Comments first: several of them quote the pixel sizes this replaced, and a
	// check that reads its own explanation as a violation is worse than no check.
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")

	// The declaration's own value, not the line it sits on. A first attempt matched
	// any px on a line containing font-size and flagged a `border-left: 3px`
	// alongside one, which is a border and has nothing to do with type.
	decl := regexp.MustCompile(`font(?:-size)?:\s*([^;}]*)`)
	for _, m := range decl.FindAllStringSubmatch(stripped, -1) {
		value := strings.TrimSpace(m[1])
		if regexp.MustCompile(`\d+px`).MatchString(value) {
			t.Errorf("a type size is pinned in pixels, so the text-size preference cannot reach it:\n  font-size: %s", value)
		}
	}
}

func readStylesheet(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("static", "tome.css"))
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	return string(b)
}

// Layout widths must not move with the text size. Asserted because the failure is
// invisible here — no browser computes CSS in this suite — and because the natural
// thing to write is a plain rem width, which is exactly what this replaced.
//
// The reasoning is the maintainer's, after using it: they preferred the column the
// largest step produced and wanted it at every step. It reverses the "keep 68
// characters per line" reasoning the feature started with, so a later reader of
// this stylesheet needs to know the current behavior is chosen rather than
// accidental.
func TestLayoutWidthsDoNotMoveWithTheTextSize(t *testing.T) {
	css := readStylesheet(t)

	if !strings.Contains(css, "--layout: calc(1.32 / var(--text-scale, 1))") {
		t.Error("the layout factor that cancels the text scale is missing")
	}

	// Breakpoints are exempt and must be, so their conditions come out before the
	// scan: `rem` inside a media query resolves against the browser's initial font
	// size rather than the root element's, so a text size cannot move one. A first
	// version of this check flagged `@media (max-width: 34rem)` and would have
	// pushed someone into "fixing" a value that was already stable.
	inRules := regexp.MustCompile(`@media[^{]*`).ReplaceAllString(css, "")

	// Every page container, and the shared measure. A bare rem here is a width that
	// grows with the type again.
	bare := regexp.MustCompile(`max-width:\s*\d+(\.\d+)?rem`)
	for _, m := range bare.FindAllString(inRules, -1) {
		// The sign-in box is deliberately excluded: nobody is signed in there, so
		// there is no chosen size for it to honor.
		if strings.Contains(m, "22rem") || strings.Contains(m, "17rem") {
			continue
		}
		t.Errorf("a container width is in bare rem, so it grows with the text: %s", m)
	}

	if !strings.Contains(css, "--measure: calc(34rem * var(--layout))") {
		t.Error("the shared measure still scales with the text size")
	}
}
