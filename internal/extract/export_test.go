package extract

import (
	"strings"

	"golang.org/x/net/html"
)

// TextForTest exposes the visible-text extraction to this package's tests.
//
// Exported for tests only, and from a file that says so: the boundary rules it
// implements are worth asserting directly, and the alternative is asserting them
// through a full extraction where a failure could be any of five rungs.
func TextForTest(raw []byte) string { return textOf(raw) }

// PruneForTest runs the unseen-content pass over a page and returns what is left.
func PruneForTest(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	pruneUnseen(doc)
	var b strings.Builder
	_ = html.Render(&b, doc)
	return b.String()
}

// MarkCaptionsForTest runs caption marking over a page and returns what is left.
func MarkCaptionsForTest(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	markCaptions(doc)
	var b strings.Builder
	_ = html.Render(&b, doc)
	return b.String()
}
