package archive_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/runlevel-six/tomekeeper/internal/archive"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

func newWriter(t *testing.T) (*archive.Writer, *blob.Filesystem) {
	t.Helper()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	return archive.NewWriter(blobs), blobs
}

const articleDir = "articles/2026/08/the-article-a1b2c3d4"

func sampleArticle() archive.Article {
	published := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)

	return archive.Article{
		Dir:         articleDir,
		URL:         "https://example.com/the-article",
		Title:       "The Article",
		Author:      "Dana Okonkwo",
		SiteName:    "Example Journal",
		Language:    "en",
		PublishedAt: &published,
		ArchivedAt:  time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		ContentHTML: `<p>An opening paragraph.</p>` +
			`<figure><img src="/assets/sha256/a1/b2/a1b2c3.avif" alt="A photograph">` +
			`<figcaption>A caption.</figcaption></figure>` +
			`<p>A closing paragraph with <a href="https://example.com/elsewhere">a link</a>.</p>`,
		WordCount:        14,
		Extractor:        "trafilatura",
		ExtractorVersion: "1",
		HasRaw:           true,
		Assets: []exchange.Asset{{
			Path:      "assets/sha256/a1/b2/a1b2c3.avif",
			SourceURL: "https://example.com/photo.jpg",
			SHA256:    "a1b2c3",
			MediaType: "image/avif",
			ByteSize:  40960,
			Width:     1600,
			Height:    900,
		}},
	}
}

// The M3 acceptance criterion, and the testing strategy's non-negotiable test.
//
// A generated index.html is opened from a temporary directory with no server
// running, and every image it references must resolve to a file that actually
// exists on disk. If this fails, the archive has stopped being an archive and
// has become a cache.
func TestStandaloneIndexResolvesImages(t *testing.T) {
	w, blobs := newWriter(t)
	ctx := t.Context()

	// Put a real asset file where the article will point.
	const assetPath = "assets/sha256/a1/b2/a1b2c3.avif"
	if err := blobs.Put(ctx, assetPath, strings.NewReader("pretend AVIF bytes")); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	if err := w.Write(ctx, sampleArticle()); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	// From here on, nothing but the filesystem. No blob store, no database, no
	// server — exactly what a person opening the archive in 2036 has.
	indexPath := filepath.Join(blobs.Root(), articleDir, archive.IndexFile)

	f, err := os.Open(indexPath)
	if err != nil {
		t.Fatalf("opening the archived page: %v", err)
	}
	defer func() { _ = f.Close() }()

	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatalf("parsing the archived page: %v", err)
	}

	images := doc.Find("img[src]")
	if images.Length() == 0 {
		t.Fatal("the archived page has no images; the fixture has one")
	}

	images.Each(func(_ int, img *goquery.Selection) {
		src, _ := img.Attr("src")

		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
			t.Errorf("image src %q is absolute: the archive depends on a server that may be gone", src)
			return
		}
		if strings.HasPrefix(src, "/") {
			t.Errorf("image src %q is root-relative: it resolves to nothing when opened from a file", src)
			return
		}

		// Resolve exactly as a browser would: relative to the page's own
		// directory.
		resolved := filepath.Join(filepath.Dir(indexPath), filepath.FromSlash(src))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("image src %q does not resolve to a file: %v\n  page:     %s\n  resolved: %s",
				src, err, indexPath, resolved)
		}
	})
}

// The exact relative depth, spelled out. articles/YYYY/MM/slug is four levels
// down, so the prefix is four "../" segments.
func TestRelativizeAssets(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		body string
		want string
	}{
		{
			name: "four levels deep",
			dir:  "articles/2026/08/slug-abc123",
			body: `<img src="/assets/sha256/a1/b2/x.avif"/>`,
			want: `<img src="../../../../assets/sha256/a1/b2/x.avif"/>`,
		},
		{
			name: "srcset candidates are each rewritten",
			dir:  "articles/2026/08/slug-abc123",
			body: `<img srcset="/assets/sha256/a1/b2/x.avif 800w, /assets/sha256/c3/d4/y.avif 1600w"/>`,
			want: `srcset="../../../../assets/sha256/a1/b2/x.avif 800w, ../../../../assets/sha256/c3/d4/y.avif 1600w"`,
		},
		{
			name: "picture sources",
			dir:  "articles/2026/08/slug-abc123",
			body: `<picture><source src="/assets/sha256/a1/b2/x.avif"/><img src="/assets/sha256/c3/d4/y.avif"/></picture>`,
			want: `../../../../assets/sha256/c3/d4/y.avif`,
		},
		{
			name: "external references are left alone",
			dir:  "articles/2026/08/slug-abc123",
			body: `<img src="https://example.com/not-localized.jpg"/>`,
			want: `<img src="https://example.com/not-localized.jpg"/>`,
		},
		{
			name: "links are not asset references",
			dir:  "articles/2026/08/slug-abc123",
			body: `<a href="/assets/somewhere">text</a>`,
			want: `<a href="/assets/somewhere">text</a>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := archive.RelativizeAssets(tt.body, tt.dir)
			if !strings.Contains(got, tt.want) {
				t.Errorf("RelativizeAssets()\n got: %s\nwant it to contain: %s", got, tt.want)
			}
		})
	}
}

// An image that could not be localized keeps its absolute URL, so the page
// still shows something for as long as the origin lives.
func TestUnlocalizedImageKeepsAbsoluteURL(t *testing.T) {
	w, blobs := newWriter(t)

	a := sampleArticle()
	a.ContentHTML = `<p>Text.</p><img src="https://cdn.example.com/failed.jpg" alt="not localized">`
	a.Assets = nil

	if err := w.Write(t.Context(), a); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	page := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.IndexFile))
	if !strings.Contains(page, "https://cdn.example.com/failed.jpg") {
		t.Error("the un-localized image was dropped; it should stay as an absolute URL")
	}
}

func TestIndexIsSelfContained(t *testing.T) {
	w, blobs := newWriter(t)

	if err := w.Write(t.Context(), sampleArticle()); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	page := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.IndexFile))

	// Nothing may be fetched to render this page. Each of these is a
	// dependency on something outliving the archive.
	forbidden := []struct{ fragment, why string }{
		{"<link ", "a stylesheet link is an external dependency"},
		{"<script", "a script is both an external dependency and an execution risk"},
		{"@import", "a CSS import fetches from the network"},
		{"fonts.googleapis", "a webfont is a dependency on a third party"},
	}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(page), f.fragment) {
			t.Errorf("index.html contains %q: %s", f.fragment, f.why)
		}
	}

	// And the article itself has to be there.
	for _, want := range []string{"The Article", "An opening paragraph", "Dana Okonkwo", "A caption"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}

	// The original URL stays visible: an archived page that hides where it
	// came from is a page you cannot check.
	if !strings.Contains(page, "https://example.com/the-article") {
		t.Error("index.html does not show the original URL")
	}
}

func TestMetaJSON(t *testing.T) {
	w, blobs := newWriter(t)

	if err := w.Write(t.Context(), sampleArticle()); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	raw := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.MetaFile))

	record, err := exchange.Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}

	if record.SchemaVersion != exchange.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", record.SchemaVersion, exchange.SchemaVersion)
	}
	if record.URL != "https://example.com/the-article" {
		t.Errorf("url = %q", record.URL)
	}
	if record.Title != "The Article" {
		t.Errorf("title = %q", record.Title)
	}
	if record.RawHTMLPath != archive.RawFile {
		t.Errorf("raw_html_path = %q, want %q", record.RawHTMLPath, archive.RawFile)
	}
	if len(record.Assets) != 1 {
		t.Fatalf("the record has %d assets, want 1", len(record.Assets))
	}

	// Asset paths in the record are relative to the record's own directory,
	// so a copied or moved archive still resolves.
	asset := record.Assets[0]
	if !strings.HasPrefix(asset.Path, "../../../../assets/") {
		t.Errorf("asset path = %q, want it relative to the article directory", asset.Path)
	}
	if asset.SourceURL != "https://example.com/photo.jpg" {
		t.Errorf("source_url = %q, want the provenance kept", asset.SourceURL)
	}

	// Readable in a text editor, which is a realistic way for this to be used.
	if !strings.Contains(raw, "\n  \"url\"") {
		t.Error("meta.json is not indented; it is meant to be read by a person")
	}
}

// A future reader must not silently misinterpret a file a newer version wrote.
func TestExchangeRejectsNewerSchema(t *testing.T) {
	record := map[string]any{
		"schema_version": exchange.SchemaVersion + 1,
		"url":            "https://example.com/x",
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	if _, err := exchange.Decode(strings.NewReader(string(encoded))); err == nil {
		t.Error("Decode() accepted a newer schema version")
	} else if !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("Decode() error = %q, want it to tell the reader what to do", err)
	}
}

func TestExchangeRejectsNonRecords(t *testing.T) {
	for _, input := range []string{
		`{"url": "https://example.com/x"}`, // no schema_version
		`{"schema_version": 1}`,            // no url
		`not json at all`,
	} {
		if _, err := exchange.Decode(strings.NewReader(input)); err == nil {
			t.Errorf("Decode(%q) = nil, want an error", input)
		}
	}
}

// Regenerating is always safe: index.html and meta.json are derived from the
// database and the assets tree, which is what lets reextract refresh them.
func TestWriteIsIdempotent(t *testing.T) {
	w, blobs := newWriter(t)
	ctx := t.Context()

	if err := w.Write(ctx, sampleArticle()); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	first := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.IndexFile))

	if err := w.Write(ctx, sampleArticle()); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	second := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.IndexFile))

	if first != second {
		t.Error("writing the same article twice produced different pages")
	}

	// And nothing was left behind beside the two files and the asset tree.
	entries, err := os.ReadDir(filepath.Join(blobs.Root(), articleDir))
	if err != nil {
		t.Fatalf("reading the article directory: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the article directory holds %v, want index.html and meta.json", names)
	}
}

func TestUntitledArticleFallsBackToItsURL(t *testing.T) {
	w, blobs := newWriter(t)

	a := sampleArticle()
	a.Title = ""

	if err := w.Write(t.Context(), a); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	page := readFile(t, filepath.Join(blobs.Root(), articleDir, archive.IndexFile))
	if !strings.Contains(page, "<title>https://example.com/the-article</title>") {
		t.Error("an untitled article has no usable <title>")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}
