package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
)

// The archive layout:
//
//	<root>/
//	  articles/2026/08/the-article-slug-a1b2c3/
//	      index.html        # standalone, relative asset links   (M3)
//	      meta.json         # the export record                  (M3)
//	      raw.html.gz       # the original fetched page           (M2)
//	  assets/sha256/a1/b2/a1b2c3….avif                            (M3)
//
// The tree is meant to be navigated by a human with a file manager, ten years
// from now, with this service long gone. That is why the directory carries a
// readable slug rather than only a hash, and why it is dated.

const (
	// slugMaxLength keeps directory names readable and well clear of the
	// 255-byte limit most filesystems impose on a single component.
	slugMaxLength = 60

	// shortHashLength is enough to make collisions implausible across an
	// archive of a few hundred thousand articles while staying readable.
	shortHashLength = 8
)

// ArticleDir returns the directory holding one article's files.
//
// The date is when the article entered the archive, not when it was published.
// Publication dates are frequently missing and occasionally absurd, and an
// archive keyed on them scatters a decade-old article saved today into a
// directory nobody thinks to look in. First-seen makes the tree append-only,
// so "what did I archive last August" is answerable by listing a directory.
//
// The trailing hash is derived from the canonical URL, which never changes for
// a given article. Titles do change — a headline is edited, a feed supplies a
// better one later — so the slug alone could not identify a directory stably.
func ArticleDir(firstSeen time.Time, title, canonicalURL string) string {
	utc := firstSeen.UTC()
	if firstSeen.IsZero() {
		// Defensive: an article with no first-seen date still needs somewhere
		// to live, and a zero year would sort strangely forever.
		utc = time.Now().UTC()
	}

	name := slug(title)
	if name == "" {
		name = slug(slugFromURL(canonicalURL))
	}
	if name == "" {
		name = "article"
	}

	return path.Join(
		"articles",
		fmt.Sprintf("%04d", utc.Year()),
		fmt.Sprintf("%02d", int(utc.Month())),
		name+"-"+shortHash(canonicalURL),
	)
}

// RawPath returns where an article's original fetched page is stored.
func RawPath(firstSeen time.Time, title, canonicalURL string) string {
	return path.Join(ArticleDir(firstSeen, title, canonicalURL), "raw.html.gz")
}

// slug turns a title into a filesystem-safe, readable directory component.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	lastWasDash := true // leading dashes are suppressed
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// Letters beyond ASCII are kept: an archive of a Japanese or
			// Greek site should have readable directory names too, and every
			// filesystem this runs on stores UTF-8 names.
			b.WriteRune(r)
			lastWasDash = false
		case !lastWasDash:
			b.WriteByte('-')
			lastWasDash = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if len(out) <= slugMaxLength {
		return out
	}

	// Truncate on a rune boundary, then back up to the last separator so the
	// name does not end mid-word.
	out = out[:slugMaxLength]
	for len(out) > 0 && !isValidCut(out) {
		out = out[:len(out)-1]
	}
	if i := strings.LastIndexByte(out, '-'); i > slugMaxLength/2 {
		out = out[:i]
	}
	return strings.Trim(out, "-")
}

// isValidCut reports whether s ends on a UTF-8 rune boundary.
func isValidCut(s string) bool {
	if s == "" {
		return true
	}
	r := s[len(s)-1]
	return r < 0x80 || !isContinuation(r)
}

func isContinuation(b byte) bool { return b&0xC0 == 0x80 }

// slugFromURL recovers something readable from a URL when there is no title.
func slugFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	trimmed := strings.Trim(u.Path, "/")
	if trimmed == "" {
		return u.Hostname()
	}

	segments := strings.Split(trimmed, "/")
	last := segments[len(segments)-1]

	// Drop a file extension: "the-post.html" reads better as "the-post".
	if i := strings.LastIndexByte(last, '.'); i > 0 {
		last = last[:i]
	}
	return last
}

// shortHash is a stable identifier for a URL.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:shortHashLength]
}
