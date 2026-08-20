package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// Tomekeeper reads this archive's own export.
//
// The adapter that makes the format's symmetry real rather than claimed. Export
// emits these records; this reads them; one round-trip test therefore exercises
// both directions, and there is no second format for a backup to be written in and
// silently rot.
//
// It is also what makes a restore a restore. Where another system's library arrives
// as an import — bodies recorded as immutable, because they may be the only copy
// left — a record from here states its own provenance, and is reproduced with it.
type Tomekeeper struct{}

// SourceTomekeeper is the name recorded against articles restored from an export
// that did not already carry a source of their own.
const SourceTomekeeper = "tomekeeper"

func (Tomekeeper) Name() string { return SourceTomekeeper }

// Detect recognizes the format by its version field.
//
// `schema_version` is this format's own vocabulary and appears in every record;
// combined with the array and a `url`, nothing else this build reads looks like it.
// A Wallabag export has no schema_version, so the two cannot be confused.
func (Tomekeeper) Detect(head []byte) bool {
	if !bytes.HasPrefix(bytes.TrimSpace(head), []byte("[")) {
		return false
	}
	return bytes.Contains(head, []byte(`"schema_version"`)) &&
		bytes.Contains(head, []byte(`"url"`))
}

// Import streams the export, yielding one article per record.
//
// The records are already in this package's format, so there is no mapping — only
// validation, which is the part that matters when reading a file written by a
// version of this program that no longer exists.
func (Tomekeeper) Import(ctx context.Context, src Source) iter.Seq2[*Article, error] {
	return func(yield func(*Article, error) bool) {
		dec := json.NewDecoder(src.Reader)

		tok, err := dec.Token()
		if err != nil {
			yield(nil, fatal(fmt.Errorf("reading %s: %w", src.Path, err)))
			return
		}
		if delim, ok := tok.(json.Delim); !ok || delim != '[' {
			yield(nil, fatal(fmt.Errorf("%s is not an export: it does not begin with an array", src.Path)))
			return
		}

		for index := 0; dec.More(); index++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			var article Article
			if err := dec.Decode(&article); err != nil {
				// The same three cases the Wallabag adapter distinguishes, and for the
				// same reasons: an exhausted input is the file being truncated, a syntax
				// error leaves the position unknown, and anything else is one bad record
				// in a file that is otherwise readable.
				if isIncomplete(err) {
					yield(nil, fatal(fmt.Errorf("%s ends before its last record: %w", src.Path, err)))
					return
				}
				if isUnrecoverable(err) {
					yield(nil, fatal(fmt.Errorf("record %d of %s: %w", index+1, src.Path, err)))
					return
				}
				if !yield(nil, fmt.Errorf("record %d of %s: %w", index+1, src.Path, err)) {
					return
				}
				continue
			}

			if err := validate(article); err != nil {
				// A record this build must not guess at. Reported per record rather
				// than fatally: one unreadable entry in a restore should cost that
				// entry, and a *file* written by a newer build fails every record,
				// which says the same thing more loudly.
				if !yield(nil, fmt.Errorf("record %d of %s: %w", index+1, src.Path, err)) {
					return
				}
				continue
			}

			// An article restored from an export that never came from anywhere else
			// is this archive's own. Named so that the import bookkeeping has a key,
			// and left alone when the record already carries the source it came from
			// — a Wallabag article restored here is still a Wallabag article, and a
			// later re-import of that library must still recognize it.
			if article.SourceName == "" {
				article.SourceName = SourceTomekeeper
			}

			if !yield(&article, nil) {
				return
			}
		}

		if _, err := dec.Token(); err != nil {
			yield(nil, fatal(fmt.Errorf("%s ends before its last record: %w", src.Path, err)))
		}
	}
}
