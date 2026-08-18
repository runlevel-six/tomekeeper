package exchange

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"strings"
)

// Importer reads one other system's export and yields articles in this
// package's format.
//
// Compiled in rather than loaded as a plugin, and kept thin on purpose: there is
// no configuration, no options struct, and nothing an adapter can decide about
// how the archive stores what it produces. An adapter's whole job is to map one
// export's fields onto Article, which is why a new one is a file and a fixture
// rather than a subsystem.
type Importer interface {
	// Name is the source system, recorded on every article this adapter imports
	// and used as half the idempotency key. It must be stable forever: changing
	// it makes every previous import invisible and re-imports the whole library
	// as new articles.
	Name() string

	// Detect reports whether a file looks like this adapter's format, so that
	// `tome import ./export.json` needs no --format flag. It examines the file's
	// first few kilobytes rather than parsing it, because detection runs on files
	// this adapter is about to decline.
	Detect(path string) (bool, error)

	// Import yields one article per record.
	//
	// A record that cannot be read yields a nil article and an error, and
	// iteration continues: one malformed entry in a ten-year library must not
	// hide the other nine thousand. A failure that ends the file — a truncated
	// or corrupt document — yields an error and stops, because everything after
	// it is unknown rather than merely broken.
	Import(ctx context.Context, src Source) iter.Seq2[*Article, error]
}

// FatalError marks a failure that ends the file rather than one record.
//
// The distinction is the difference between "one entry of your library is
// unreadable" and "the second half of your library is unknown". An adapter wraps
// the second kind, and the import stops and fails rather than reporting a partial
// success over a file it could not finish reading — which is the failure mode that
// would quietly lose half an archive.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

// fatal wraps an error as file-ending.
func fatal(err error) error { return &FatalError{Err: err} }

// isFatal reports whether an error ended the file.
func isFatal(err error) bool {
	var f *FatalError
	return errors.As(err, &f)
}

// Source is one export to read.
type Source struct {
	// Path is where the export came from. Used in error messages, and it is what
	// Detect examines.
	Path string

	// Reader supplies the bytes.
	//
	// A reader rather than a path so that an import can be driven from an upload
	// or a test fixture, and so that the two passes over an export — inspect,
	// then write — are each handed a fresh one by the caller rather than seeking
	// a file the caller may not own.
	Reader io.Reader
}

// Importers is every adapter compiled into this build, in the order Detect tries
// them.
func Importers() []Importer {
	return []Importer{Wallabag{}}
}

// DetectImporter finds the adapter that recognizes a file.
//
// Reports a nil Importer and no error when nothing recognizes it: not knowing a
// format is an ordinary outcome that the caller turns into a message naming the
// formats it does know, which is more use than an error saying one file failed.
func DetectImporter(path string) (Importer, error) {
	for _, imp := range Importers() {
		ok, err := imp.Detect(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return imp, nil
		}
	}
	return nil, nil
}

// ImporterNamed finds an adapter by name, for `--format`.
func ImporterNamed(name string) (Importer, bool) {
	for _, imp := range Importers() {
		if strings.EqualFold(imp.Name(), name) {
			return imp, true
		}
	}
	return nil, false
}

// ImporterNames lists what this build can read, for a usage message.
func ImporterNames() []string {
	out := make([]string, 0, len(Importers()))
	for _, imp := range Importers() {
		out = append(out, imp.Name())
	}
	return out
}

// detectHead is how much of a file Detect may read.
//
// Enough to see a JSON export's first record, which is where the field names
// that identify a format are. Bounded because detection is run against files
// that are about to be rejected, and one of them being a DVD image should not
// cost anything.
const detectHead = 8 << 10

// headOf reads the first detectHead bytes of a file.
//
// A missing or unreadable file is an error rather than "not this format": the
// operator named it, so being unable to open it is worth saying plainly instead
// of reporting it as an unrecognized format.
func headOf(path string) ([]byte, error) {
	// G304 wants a constant path; a variable one is the entire command. The
	// operator names the file to import and nothing here is remotely reachable.
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, detectHead)
	n, err := io.ReadFull(f, head)
	// A short file is the ordinary case, not a failure: an export smaller than the
	// window this reads still has to be detectable.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("reading the start of %s: %w", path, err)
	}
	return head[:n], nil
}
