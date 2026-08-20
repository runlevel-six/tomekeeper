// Package archive writes the on-disk form of an article.
//
// Files are the archive, the database is an index. Everything
// here exists to make that literally true — a directory per article holding a
// standalone index.html, the record as meta.json, and the original page as
// raw.html.gz, with images resolved by relative path into a shared assets tree.
//
// The test that matters opens a generated index.html from a temporary
// directory with nothing running and asserts that every image resolves. If
// that test ever fails, the archive has stopped being an archive and has
// become a cache.
package archive

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"path"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

// Names of the files in an article's directory.
const (
	IndexFile = "index.html"
	MetaFile  = "meta.json"
	RawFile   = "raw.html.gz"
)

// AssetURLPrefix is how asset references are stored in the database.
//
// The stored body uses a root-relative path, which is what a web server can
// route directly. index.html needs a *file*-relative path instead, because it
// is opened from disk where there is no root. Writer rewrites one into the
// other; this constant is the seam between them.
const AssetURLPrefix = "/assets/"

// Writer produces the on-disk form of articles.
type Writer struct {
	blobs blob.Store
}

// NewWriter returns a Writer over a blob store.
func NewWriter(blobs blob.Store) *Writer {
	return &Writer{blobs: blobs}
}

// Article is everything needed to write one article's files.
type Article struct {
	Dir string // the article's directory, from blob.ArticleDir

	URL         string
	Title       string
	Author      string
	SiteName    string
	Language    string
	PublishedAt *time.Time
	ArchivedAt  time.Time

	// ContentHTML is the sanitized body with root-relative asset references.
	ContentHTML string
	WordCount   int
	Excerpt     string

	Extractor        string
	ExtractorVersion string
	Immutable        bool

	// HasRaw reports whether raw.html.gz sits beside these files.
	HasRaw bool

	Assets  []exchange.Asset
	Tags    []string
	Read    bool
	Starred bool
}

// Write generates index.html and meta.json in the article's directory.
//
// Both are rewritten from scratch each time. They are derived artifacts — the
// database and the assets tree are the inputs — so regenerating is always safe
// and is what makes `tome reextract` able to refresh the readable archive.
func (w *Writer) Write(ctx context.Context, a Article) error {
	if a.Dir == "" {
		return fmt.Errorf("the article has no directory")
	}

	index, err := w.renderIndex(a)
	if err != nil {
		return err
	}
	if err := w.blobs.Put(ctx, path.Join(a.Dir, IndexFile), bytes.NewReader(index)); err != nil {
		return fmt.Errorf("writing %s: %w", IndexFile, err)
	}

	meta, err := renderMeta(a)
	if err != nil {
		return err
	}
	if err := w.blobs.Put(ctx, path.Join(a.Dir, MetaFile), bytes.NewReader(meta)); err != nil {
		return fmt.Errorf("writing %s: %w", MetaFile, err)
	}
	return nil
}

// templateData is what the page template sees.
type templateData struct {
	Title         string
	Author        string
	SiteName      string
	Language      string
	URL           string
	PublishedDate string
	ArchivedDate  string
	WordCount     int
	ContentHTML   template.HTML
}

func (w *Writer) renderIndex(a Article) ([]byte, error) {
	body := RelativizeAssets(a.ContentHTML, a.Dir)

	title := a.Title
	if strings.TrimSpace(title) == "" {
		title = a.URL
	}

	data := templateData{
		Title:        title,
		Author:       a.Author,
		SiteName:     a.SiteName,
		Language:     a.Language,
		URL:          a.URL,
		ArchivedDate: a.ArchivedAt.UTC().Format("2 January 2006"),
		WordCount:    a.WordCount,
		// The body was sanitized by internal/extract before it was stored, and
		// is trusted here on that basis. This is the one place the archive
		// declares HTML safe, so it is worth stating why: escaping it again
		// would render the article as visible markup rather than as an article.
		ContentHTML: template.HTML(body), //nolint:gosec // sanitized at extraction
	}
	if a.PublishedAt != nil {
		data.PublishedDate = a.PublishedAt.UTC().Format("2 January 2006")
	}

	var buf bytes.Buffer
	if err := articleTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering the article page: %w", err)
	}
	return buf.Bytes(), nil
}

func renderMeta(a Article) ([]byte, error) {
	record := exchange.Article{
		URL:              a.URL,
		Title:            a.Title,
		Author:           a.Author,
		SiteName:         a.SiteName,
		Language:         a.Language,
		PublishedAt:      a.PublishedAt,
		SavedAt:          &a.ArchivedAt,
		ContentHTML:      RelativizeAssets(a.ContentHTML, a.Dir),
		Excerpt:          a.Excerpt,
		Tags:             a.Tags,
		Read:             a.Read,
		Starred:          a.Starred,
		Extractor:        a.Extractor,
		ExtractorVersion: a.ExtractorVersion,
		Immutable:        a.Immutable,
		Assets:           relativizeAssetRecords(a.Assets, a.Dir),
		WordCount:        a.WordCount,
	}
	if a.HasRaw {
		record.RawHTMLPath = RawFile
	}

	var buf bytes.Buffer
	if err := exchange.Encode(&buf, record); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RelativizeAssets rewrites root-relative asset references into paths relative
// to an article's directory.
//
// This is what makes index.html work when opened from a file manager. A
// reference to /assets/sha256/a1/b2/… means nothing to a browser reading
// file:///home/someone/archive/articles/2026/08/slug-abc123/index.html; the
// same asset has to be addressed as ../../../../assets/sha256/a1/b2/….
//
// Getting this wrong is not a cosmetic bug. It is the difference between an
// archive and a folder of pages with broken images.
func RelativizeAssets(body, articleDir string) string {
	prefix := relativePrefix(articleDir)
	if prefix == "" || body == "" {
		return body
	}

	return MapAssetRefs(body, func(value string) string {
		return prefix + strings.TrimPrefix(value, "/")
	})
}

// MapAssetRefs rewrites every root-relative asset reference in a body through
// rewrite, and leaves everything else alone.
//
// One definition of *where* an asset reference can appear, shared by the two
// callers that need to move them. RelativizeAssets above turns them into paths a
// browser can follow from a file:// URL; the Fever API turns them into absolute
// signed URLs a mobile client can fetch without a session. The transformations have
// nothing in common, but the set of attributes does — and srcset in particular is
// fiddly enough that a second copy of this traversal would be a second chance to
// get it wrong, in a way whose only symptom is a missing picture.
//
// rewrite is called with the reference as stored, always beginning with
// AssetURLPrefix. A body that cannot be parsed is returned unchanged, which is the
// same failure this has always taken: an unrewritten reference shows a broken image,
// while a half-rewritten body could show the wrong one.
func MapAssetRefs(body string, rewrite func(string) string) string {
	if body == "" {
		return body
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return body
	}

	// Guarded here rather than in every caller, so that a caller's rewrite function
	// only ever sees the references it is entitled to change.
	guarded := func(value string) string {
		if !strings.HasPrefix(value, AssetURLPrefix) {
			return value
		}
		return rewrite(value)
	}

	doc.Find("img[src], source[src], video[src], audio[src]").Each(func(_ int, node *goquery.Selection) {
		if value, ok := node.Attr("src"); ok {
			node.SetAttr("src", guarded(value))
		}
	})

	doc.Find("[srcset]").Each(func(_ int, node *goquery.Selection) {
		value, ok := node.Attr("srcset")
		if !ok {
			return
		}
		candidates := strings.Split(value, ",")
		for i, candidate := range candidates {
			fields := strings.Fields(strings.TrimSpace(candidate))
			if len(fields) == 0 {
				continue
			}
			fields[0] = guarded(fields[0])
			candidates[i] = strings.Join(fields, " ")
		}
		node.SetAttr("srcset", strings.Join(candidates, ", "))
	})

	out, err := doc.Find("body").Html()
	if err != nil {
		return body
	}
	return out
}

// relativePrefix returns the "../" sequence that climbs from an article's
// directory back to the archive root.
func relativePrefix(articleDir string) string {
	clean := strings.Trim(path.Clean(articleDir), "/")
	if clean == "" || clean == "." {
		return ""
	}
	return strings.Repeat("../", strings.Count(clean, "/")+1)
}

func relativizeAssetRecords(assets []exchange.Asset, articleDir string) []exchange.Asset {
	if len(assets) == 0 {
		return nil
	}

	prefix := relativePrefix(articleDir)
	out := make([]exchange.Asset, 0, len(assets))

	for _, a := range assets {
		if strings.HasPrefix(a.Path, AssetURLPrefix) {
			a.Path = prefix + strings.TrimPrefix(a.Path, "/")
		} else if !strings.HasPrefix(a.Path, "..") && !strings.HasPrefix(a.Path, "/") {
			// A store-relative path such as assets/sha256/… needs the same
			// treatment; it just arrives without the leading slash.
			a.Path = prefix + a.Path
		}
		out = append(out, a)
	}
	return out
}
