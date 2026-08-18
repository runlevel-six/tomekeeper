// Package exchange defines the interchange format for articles entering and
// leaving the archive.
//
// One struct serves both directions. Importers produce it, exports emit it,
// and `meta.json` in every article's directory is its serialization. That
// symmetry is deliberate insurance: because export
// uses the same type every importer consumes, export is exercised by every
// import test and cannot quietly rot. For a project with one maintainer, a
// format that only breaks when someone finally needs it is the format that
// loses the archive.
//
// The type is defined with the archive writer rather than with the importers that
// will consume it, because the writer cannot produce `meta.json` without it. The
// importers consume this
// format; it does not get to invent one.
package exchange

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SchemaVersion is the version of this format.
//
// Bump it for any change that an older reader could misinterpret: a removed
// field, a renamed field, or a changed meaning. Adding an optional field does
// not require a bump, because an older reader ignoring a field it does not
// know about is the intended behavior.
//
// Every bump needs a note in docs/reference/export-format.md saying what
// changed and how to read the older version. An archive holds files written by
// every version this service has ever run.
const SchemaVersion = 1

// Article is one article in the interchange format.
//
// Field names are chosen to be readable in a text editor a decade from now,
// because that is a realistic way for this file to be used: `meta.json` sitting
// beside an `index.html` is often all the context a future reader has.
type Article struct {
	SchemaVersion int `json:"schema_version"`

	// SourceID is the identifier in the system this came from, used to make
	// re-import idempotent. Empty for articles this archive fetched itself.
	SourceID string `json:"source_id,omitempty"`

	// SourceName is the system it came from: "wallabag", "pocket", and so on.
	// Empty for articles this archive fetched itself.
	SourceName string `json:"source_name,omitempty"`

	URL         string `json:"url"`
	ResolvedURL string `json:"resolved_url,omitempty"`

	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
	SiteName string `json:"site_name,omitempty"`
	Language string `json:"language,omitempty"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	SavedAt     *time.Time `json:"saved_at,omitempty"`

	// ContentHTML is the extracted body. Image references point at the paths
	// in Assets, relative to this file's directory, so that the record and the
	// files beside it are consistent when read together.
	ContentHTML string `json:"content_html,omitempty"`

	// RawHTML is the original page, when the source kept one.
	//
	// It is a *path* rather than inline bytes in the on-disk form: a decade of
	// raw pages inlined into JSON would make every meta.json unreadable in a
	// text editor and every export a single enormous document.
	RawHTMLPath string `json:"raw_html_path,omitempty"`

	Excerpt string   `json:"excerpt,omitempty"`
	Tags    []string `json:"tags,omitempty"`

	Read     bool `json:"read"`
	Starred  bool `json:"starred"`
	Archived bool `json:"archived"`

	// Extraction records which extractor produced ContentHTML, so that an
	// imported body can be told apart from one this archive extracted.
	Extractor        string `json:"extractor,omitempty"`
	ExtractorVersion string `json:"extractor_version,omitempty"`

	// Immutable marks a body that must never be regenerated — typically an
	// import that is the only surviving copy of a dead URL.
	Immutable bool `json:"immutable,omitempty"`

	Highlights []Highlight `json:"highlights,omitempty"`
	Assets     []Asset     `json:"assets,omitempty"`

	// WordCount is derived and stored anyway, because a reader browsing the
	// archive without this service running has no other way to get it.
	WordCount int `json:"word_count,omitempty"`

	// PlaceholderBody records that the source had a body field and it held the
	// source's own failure message rather than an article, so ContentHTML was left
	// empty deliberately rather than for want of anything to put there.
	//
	// Never serialized: it is a fact about a file being read, not about the
	// article, and writing it into meta.json would be recording somebody else's
	// software failing in this archive's permanent record. It exists so an import
	// report can tell "your reader never had this page" apart from "your reader had
	// no body field at all", which are different pieces of news.
	PlaceholderBody bool `json:"-"`
}

// Highlight is a passage a reader marked, with an optional note.
type Highlight struct {
	// Quote is the text itself, not a character range.
	//
	// Ranges are brittle: they are offsets into one system's rendering of the
	// body, and they stop meaning anything the moment the body is re-extracted
	// by a different extractor. The quoted text survives that.
	Quote     string     `json:"quote"`
	Note      string     `json:"note,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Asset is one localized image.
type Asset struct {
	// Path is relative to the article's directory, so that an archive copied
	// or moved wholesale still resolves.
	Path string `json:"path"`

	// SourceURL is where it came from, kept so that a lost asset can be
	// re-fetched and so that provenance is not erased by localization.
	SourceURL string `json:"source_url,omitempty"`

	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type,omitempty"`
	ByteSize  int64  `json:"byte_size,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// Encode writes an article as indented JSON.
//
// Indented on purpose: this file is meant to be opened and read by a person,
// and the few bytes of whitespace cost nothing next to the article beside it.
func Encode(w io.Writer, a Article) error {
	a.SchemaVersion = SchemaVersion

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Article URLs and titles routinely contain characters that HTML escaping
	// would mangle into < sequences, which defeats the point of a file a
	// human can read.
	enc.SetEscapeHTML(false)

	if err := enc.Encode(a); err != nil {
		return fmt.Errorf("encoding the article record: %w", err)
	}
	return nil
}

// Decode reads an article record, rejecting versions this build cannot read.
func Decode(r io.Reader) (Article, error) {
	var a Article
	if err := json.NewDecoder(r).Decode(&a); err != nil {
		return Article{}, fmt.Errorf("decoding the article record: %w", err)
	}

	if a.SchemaVersion == 0 {
		return Article{}, fmt.Errorf("the record has no schema_version; it is not an article record")
	}
	if a.SchemaVersion > SchemaVersion {
		// Refusing is kinder than guessing. A newer file may have changed the
		// meaning of a field this build thinks it understands, and silently
		// misreading an archive is worse than not reading it.
		return Article{}, fmt.Errorf(
			"the record is schema version %d and this build reads up to %d: upgrade tomekeeper",
			a.SchemaVersion, SchemaVersion)
	}
	if a.URL == "" {
		return Article{}, fmt.Errorf("the record has no url")
	}
	return a, nil
}
