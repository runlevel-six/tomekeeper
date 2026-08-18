package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"
)

// Wallabag reads a Wallabag JSON export — Settings → Export → JSON.
//
// The JSON export rather than a direct database read, and it is the only mode
// here. A database reader is faster on a large library, but Wallabag's schema has
// drifted across releases and a file is a stable artifact somebody can re-run,
// inspect, and keep. Speed is not the constraint on a one-off import of a
// personal library; being able to trust it is.
type Wallabag struct{}

// SourceWallabag is the name recorded against every article this adapter
// imports, and half of the idempotency key. Never change it.
const SourceWallabag = "wallabag"

func (Wallabag) Name() string { return SourceWallabag }

// Detect recognizes the export by the field names in its first record.
//
// Field-based rather than filename-based: the file is called whatever the browser
// saved it as. `url` and `is_archived` together are specific enough — `url` alone
// appears in half the JSON on earth, and `is_archived` is Wallabag's own
// vocabulary for what other readers call read.
func (Wallabag) Detect(head []byte) bool {
	if !bytes.HasPrefix(bytes.TrimSpace(head), []byte("[")) {
		return false
	}
	return bytes.Contains(head, []byte(`"is_archived"`)) &&
		bytes.Contains(head, []byte(`"url"`))
}

// wallabagEntry is one record of the export.
//
// Only the fields that map onto something. Wallabag also exports `headers`,
// `http_status`, `mimetype`, `is_public`, `reading_time` and `preview_picture`;
// each is either about the fetch rather than the article, or is something this
// archive derives itself and would rather derive than inherit — reading time from
// the word count it computes, and a preview image from the body it stores.
type wallabagEntry struct {
	ID    json.Number `json:"id"`
	URL   string      `json:"url"`
	Given string      `json:"given_url"`

	Title       string        `json:"title"`
	DomainName  string        `json:"domain_name"`
	Language    string        `json:"language"`
	PublishedBy []string      `json:"published_by"`
	Content     string        `json:"content"`
	CreatedAt   wallabagTime  `json:"created_at"`
	PublishedAt wallabagTime  `json:"published_at"`
	Archived    wallabagBool  `json:"is_archived"`
	Starred     wallabagBool  `json:"is_starred"`
	Tags        wallabagTags  `json:"tags"`
	Annotations []wallabagAnn `json:"annotations"`
}

type wallabagAnn struct {
	Quote     string       `json:"quote"`
	Text      string       `json:"text"`
	CreatedAt wallabagTime `json:"created_at"`
}

// wallabagBool accepts a boolean written as a boolean, a number, or a string.
//
// Wallabag has exported `is_archived` as `0`/`1` and as `false`/`true` in
// different releases, and an export is a decade-old artifact somebody may still
// be holding. Refusing to read one over the spelling of a flag would be a poor
// reason to lose a library.
type wallabagBool bool

func (b *wallabagBool) UnmarshalJSON(data []byte) error {
	switch s := strings.Trim(strings.TrimSpace(string(data)), `"`); s {
	case "true", "1":
		*b = true
	case "false", "0", "", "null":
		*b = false
	default:
		return fmt.Errorf("%q is not a boolean", s)
	}
	return nil
}

// wallabagTime accepts the timestamp formats Wallabag has used, and treats an
// unparseable one as absent.
//
// Absent rather than fatal, deliberately. A date this archive cannot read costs
// the article its published date, which is a blemish; refusing the record costs
// the article itself. The sort order falls back to when it was first seen, which
// is exactly what a saved page with no date gets anyway.
type wallabagTime struct{ Time *time.Time }

// wallabagTimeLayouts are tried in order. RFC 3339 is what current releases
// emit; the space-separated form is what the older SQL-flavored exports carry.
var wallabagTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func (t *wallabagTime) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "null" || raw == `""` || raw == "" {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// A timestamp that is not even a string. Treated as absent for the same
		// reason as one that will not parse.
		return nil //nolint:nilerr // an unreadable date costs the date, not the record
	}
	s = strings.TrimSpace(s)

	for _, layout := range wallabagTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			utc := parsed.UTC()
			t.Time = &utc
			return nil
		}
	}
	return nil
}

// wallabagTags accepts tags written as strings or as objects with a label.
type wallabagTags []string

func (t *wallabagTags) UnmarshalJSON(data []byte) error {
	var strs []string
	if err := json.Unmarshal(data, &strs); err == nil {
		*t = clean(strs)
		return nil
	}

	var objs []struct {
		Label string `json:"label"`
		Slug  string `json:"slug"`
	}
	if err := json.Unmarshal(data, &objs); err != nil {
		return fmt.Errorf("reading tags: %w", err)
	}

	names := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.Label != "" {
			names = append(names, o.Label)
			continue
		}
		names = append(names, o.Slug)
	}
	*t = clean(names)
	return nil
}

func clean(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fetchFailureMarker identifies the body Wallabag stores when its own fetch
// failed.
//
// This matters more than it looks. Wallabag writes a sentence of prose into the
// content field — "wallabag can't retrieve contents for this article", with a link
// to its troubleshooting page — and an importer that takes the field at face value
// stores that sentence as the article's body. Because an imported body is
// immutable, it would then be the *permanent* body: unreplaceable by any later
// fetch, sitting in the archive as a paragraph of someone else's error message,
// and counted as a successful import. In the maintainer's own library this is 42
// of 385 records, and every one of them is a page this archive can fetch for
// itself.
//
// Matched on the documentation URL rather than the sentence, because the sentence
// is translated and the URL is not.
const fetchFailureMarker = "doc.wallabag.org/en/user/errors_during_fetching"

// maxFetchFailureBody bounds what the marker may appear in.
//
// The marker is a placeholder that replaces the body, so it is short. Without a
// bound, a genuine article *about* Wallabag's fetch failures — which is exactly
// the kind of page in this library — would be discarded for quoting the URL.
const maxFetchFailureBody = 4 << 10

// isFetchFailure reports whether a body is Wallabag's placeholder rather than an
// article.
func isFetchFailure(body string) bool {
	return len(body) <= maxFetchFailureBody && strings.Contains(body, fetchFailureMarker)
}

// Import streams the export, yielding one article per record.
//
// Streamed rather than unmarshaled whole: a personal library is a few megabytes,
// but the format has no upper bound and the one thing an import must not do is
// fail on the biggest archive it is offered. Each record is decoded, mapped, and
// released.
func (w Wallabag) Import(ctx context.Context, src Source) iter.Seq2[*Article, error] {
	return func(yield func(*Article, error) bool) {
		dec := json.NewDecoder(src.Reader)

		// The opening bracket. A file that is not an array is not this format, and
		// saying so here is better than reporting a thousand broken records.
		tok, err := dec.Token()
		if err != nil {
			yield(nil, fatal(fmt.Errorf("reading %s: %w", src.Path, err)))
			return
		}
		if delim, ok := tok.(json.Delim); !ok || delim != '[' {
			yield(nil, fatal(fmt.Errorf("%s is not a Wallabag JSON export: it does not begin with an array", src.Path)))
			return
		}

		for index := 0; dec.More(); index++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			var entry wallabagEntry
			if err := dec.Decode(&entry); err != nil {
				// A record this decoder cannot read at all. Reported with its
				// position and skipped — but only if the decoder is still on a
				// record boundary, which Decode guarantees when the value itself
				// was well-formed JSON. A syntax error is not recoverable, because
				// the position in the file is then unknown.
				if isSyntaxError(err) {
					// The parser stopped mid-token, so the position in the file is
					// unknown and nothing after it can be trusted to be a record.
					yield(nil, fatal(fmt.Errorf("record %d of %s: %w", index+1, src.Path, err)))
					return
				}
				if !yield(nil, fmt.Errorf("record %d of %s: %w", index+1, src.Path, err)) {
					return
				}
				continue
			}

			article, err := w.mapEntry(entry, index)
			if !yield(article, err) {
				return
			}
		}

		// The closing bracket. Reaching it is how a complete file is told from one
		// that was truncated partway through — which is the difference between an
		// import that is finished and one that silently stopped early.
		if _, err := dec.Token(); err != nil {
			yield(nil, fatal(fmt.Errorf("%s ends before its last record: %w", src.Path, err)))
		}
	}
}

// isSyntaxError reports whether an error leaves the decoder's position unknown.
//
// A type mismatch inside a well-formed record is recoverable: Decode consumed
// exactly that record and the next one begins where it should. A syntax error is
// not, because the parser stopped mid-token and nothing after it can be trusted to
// be a record at all.
func isSyntaxError(err error) bool {
	var syntax *json.SyntaxError
	return errors.As(err, &syntax)
}

// mapEntry turns one Wallabag record into an Article.
//
// Every decision here is a mapping decision, and the ones that are not obvious
// carry their reasoning inline. Nothing in this function fetches, sanitizes, or
// writes: what it produces is a description of an article, and the archive
// decides what to do with it.
func (w Wallabag) mapEntry(e wallabagEntry, index int) (*Article, error) {
	// The URL as submitted, and the URL Wallabag settled on. In this library they
	// differ for 81 of 385 records — tracking parameters stripped, http upgraded
	// to https — and both are worth keeping: the archive canonicalizes for
	// identity, and the original is what the reader would recognize.
	original := strings.TrimSpace(e.Given)
	resolved := strings.TrimSpace(e.URL)
	if original == "" {
		original = resolved
	}
	if original == "" {
		return nil, fmt.Errorf("record %d has no url", index+1)
	}
	if resolved == original {
		resolved = ""
	}

	a := &Article{
		SchemaVersion: SchemaVersion,
		SourceName:    SourceWallabag,
		SourceID:      strings.TrimSpace(e.ID.String()),
		URL:           original,
		ResolvedURL:   resolved,
		Title:         strings.TrimSpace(e.Title),
		SiteName:      strings.TrimSpace(e.DomainName),
		Language:      normalizeLanguage(e.Language),
		PublishedAt:   e.PublishedAt.Time,
		SavedAt:       e.CreatedAt.Time,
		Tags:          e.Tags,

		// Wallabag's "archived" is what every other reader calls read: an entry
		// leaves the unread list when it is archived. Carried in both fields
		// because the format distinguishes them and some sources do too.
		Read:     bool(e.Archived),
		Archived: bool(e.Archived),
		Starred:  bool(e.Starred),

		// Recorded even when the body turns out to be a placeholder, so that an
		// article which arrived from an import is identifiable as one whatever
		// happened to its content.
		Extractor: "wallabag",
	}

	// published_by is a list of names; the archive has one author field, and a
	// three-way byline reads correctly as a joined string.
	a.Author = strings.Join(clean(e.PublishedBy), ", ")

	// The body, unless it is Wallabag's own failure message. Left empty in that
	// case, which is what tells the archive to fetch the page itself rather than
	// enshrining a placeholder as an immutable body.
	switch body := strings.TrimSpace(e.Content); {
	case body == "":
	case isFetchFailure(body):
		a.PlaceholderBody = true
	default:
		a.ContentHTML = body
	}

	for _, ann := range e.Annotations {
		quote := strings.TrimSpace(ann.Quote)
		if quote == "" {
			// A highlight with no quoted text cannot be reconciled against a body
			// that a different extractor produced, which is the whole reason this
			// format stores text rather than character offsets.
			continue
		}
		a.Highlights = append(a.Highlights, Highlight{
			Quote:     quote,
			Note:      strings.TrimSpace(ann.Text),
			CreatedAt: ann.CreatedAt.Time,
		})
	}

	return a, nil
}

// normalizeLanguage turns Wallabag's locale into a language tag.
//
// It exports `en`, `en_US` and `de_DE`; the underscore form is a POSIX locale
// rather than a BCP 47 tag, and it is a BCP 47 tag that belongs in HTML's lang
// attribute — which is where this value ends up.
func normalizeLanguage(raw string) string {
	return strings.ReplaceAll(strings.TrimSpace(raw), "_", "-")
}
