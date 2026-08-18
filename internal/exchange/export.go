package exchange

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// exportPage is how many articles are read per round trip.
const exportPage = 500

// ExportResult is what an export wrote.
type ExportResult struct {
	Articles   int
	Bodies     int
	Highlights int
	Tags       int
	Assets     int

	// WithoutBody is articles whose body is not in the file: a fetch that failed,
	// or one the retention policy released. Worth counting, because an export that
	// silently carried fewer bodies than the reader expected is exactly the thing
	// somebody finds out about years later.
	WithoutBody int
}

// Export writes the reader's archive as a JSON array of records.
//
// The same records the importers produce, which is the entire point and is not a
// convenience: because export emits the type every importer consumes, the format is
// exercised from both ends by one round-trip test, and an export written today can
// be read by the importer without a second format to keep in step. A format that
// only breaks when somebody finally needs it is the format that loses an archive.
//
// Streamed, one record at a time, with the array brackets written by hand. Holding
// a decade of articles in memory to marshal them in one call is the failure mode
// that makes an export tool useless at exactly the size where it matters.
//
// What is *not* in the file: image bytes and stored original pages. Those are named
// by path, because the archive on disk already holds them — an export is the
// database's half of the archive, and copying the blob tree is the other half. That
// division is stated in the how-to rather than implied, because "I exported
// everything" should not turn out to have meant the text only.
func Export(ctx context.Context, s *store.Store, userID store.UserID, w io.Writer) (ExportResult, error) {
	var result ExportResult

	buffered := bufio.NewWriterSize(w, 64<<10)

	// Each record is encoded into a small reused buffer rather than straight to the
	// output, so that the array's punctuation is this function's to place. Encoder
	// writes a trailing newline after every value, which would otherwise put each
	// separating comma on a line of its own.
	//
	// An Encoder rather than MarshalIndent because only an Encoder can be told not
	// to escape HTML, and this file is full of it: `<p>` written as `\u003cp\u003e`
	// through a decade of bodies is unreadable in an editor and larger for nothing.
	var record bytes.Buffer
	enc := json.NewEncoder(&record)
	enc.SetIndent("  ", "  ")
	enc.SetEscapeHTML(false)

	if _, err := buffered.WriteString("[\n"); err != nil {
		return result, fmt.Errorf("writing the export: %w", err)
	}

	var cursor store.ArticleID
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		rows, err := s.ExportArticles(ctx, userID, cursor, exportPage)
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			break
		}

		ids := make([]store.ArticleID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ArticleID)
		}

		tags, err := s.TagsForArticles(ctx, userID, ids)
		if err != nil {
			return result, err
		}
		highlights, err := s.HighlightsForArticles(ctx, userID, ids)
		if err != nil {
			return result, err
		}
		assets, err := s.AssetsForArticles(ctx, ids)
		if err != nil {
			return result, err
		}

		for _, row := range rows {
			cursor = row.ArticleID

			article := articleFrom(row, tags[row.ArticleID], highlights[row.ArticleID], assets[row.ArticleID])

			record.Reset()
			if err := enc.Encode(article); err != nil {
				return result, fmt.Errorf("encoding article %d: %w", row.ArticleID, err)
			}

			if result.Articles > 0 {
				if _, err := buffered.WriteString(",\n"); err != nil {
					return result, fmt.Errorf("writing the export: %w", err)
				}
			}
			if _, err := buffered.WriteString("  "); err != nil {
				return result, fmt.Errorf("writing the export: %w", err)
			}
			if _, err := buffered.Write(bytes.TrimRight(record.Bytes(), "\n")); err != nil {
				return result, fmt.Errorf("writing the export: %w", err)
			}

			result.Articles++
			if article.ContentHTML == "" {
				result.WithoutBody++
			} else {
				result.Bodies++
			}
			result.Tags += len(article.Tags)
			result.Highlights += len(article.Highlights)
			result.Assets += len(article.Assets)
		}

		if len(rows) < exportPage {
			break
		}
	}

	// The closing bracket, with a newline before it only when there is something to
	// close over. An empty archive is "[\n]" rather than a blank line in brackets:
	// still a valid document, and one that says plainly there is nothing in it.
	tail := "\n]\n"
	if result.Articles == 0 {
		tail = "]\n"
	}
	if _, err := buffered.WriteString(tail); err != nil {
		return result, fmt.Errorf("writing the export: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return result, fmt.Errorf("writing the export: %w", err)
	}

	return result, nil
}

// articleFrom assembles one export record.
//
// Every field this sets is a claim the importer will act on, so the mapping is
// deliberately literal: nothing is derived, nothing is prettied up, and the only
// judgement is which of the archive's internal facts are none of an export's
// business — the article's own id, which means nothing anywhere else, and the
// per-user rows another reader owns.
func articleFrom(row store.ExportRow, tags []string, highlights []store.ImportHighlight,
	assets []store.ExportAsset,
) Article {
	article := Article{
		SchemaVersion: SchemaVersion,

		// Provenance survives a round trip. An archive restored from this file still
		// knows which articles came from a Wallabag library, so a later re-import of
		// that library recognizes them instead of adding them a second time.
		SourceName: row.SourceName,
		SourceID:   row.SourceID,

		URL:      row.URLOriginal,
		Title:    row.Title,
		Author:   row.Author,
		SiteName: row.SiteName,
		Language: row.Language,

		PublishedAt: row.PublishedAt,
		SavedAt:     row.SavedAt,

		ContentHTML: row.ContentHTML,
		RawHTMLPath: row.RawBlobPath,

		Tags: tags,

		Read:    row.Read,
		Starred: row.Starred,

		Extractor:        row.ExtractorName,
		ExtractorVersion: row.ExtractorVersion,
		ContentOrigin:    row.ContentOrigin,
		Immutable:        row.Immutable,
		WordCount:        row.WordCount,
	}

	// An article this archive collected itself is named as its own, keyed on the
	// canonical URL — which is exactly what identity means here, since that is what
	// the archive deduplicates on. Without a key, a second restore of the same file
	// could not tell that it had already run: no duplicate articles, because the URL
	// still deduplicates, but a report claiming to have written a thousand articles
	// that were already there.
	if article.SourceName == "" {
		article.SourceName = SourceTomekeeper
		article.SourceID = row.URLCanonical
	}

	// The canonical URL is what the archive keys on, so it is the one that must
	// survive. It goes in ResolvedURL when it differs from what was originally
	// requested, which is where an importer looks for the address to key on.
	if row.URLCanonical != row.URLOriginal {
		article.ResolvedURL = row.URLCanonical
	}
	if article.URL == "" {
		article.URL = row.URLCanonical
	}

	// Excerpt is derived rather than stored, and it is what a reader browsing the
	// files sees before opening one.
	article.Excerpt = excerptOf(row.ContentText)

	for _, h := range highlights {
		article.Highlights = append(article.Highlights, Highlight{
			Quote: h.Quote, Note: h.Note, CreatedAt: h.CreatedAt,
		})
	}
	for _, a := range assets {
		article.Assets = append(article.Assets, Asset{
			Path: a.Path, SourceURL: a.SourceURL, SHA256: a.SHA256,
			MediaType: a.MediaType, ByteSize: a.ByteSize,
			Width: a.Width, Height: a.Height,
		})
	}

	return article
}

// excerptLength is how much of the body an export record carries as an excerpt.
const excerptLength = 320

// excerptOf takes the first characters of the body, on a rune boundary.
func excerptOf(text string) string {
	if len(text) <= excerptLength {
		return text
	}

	// Cut on a rune boundary rather than a byte one: an excerpt ending in half a
	// character is invalid UTF-8 in a file meant to outlive this program.
	cut := excerptLength
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut]
}

// utf8Start reports whether a byte begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
