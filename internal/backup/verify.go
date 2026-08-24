package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report is what a verification found.
type Report struct {
	Manifest Manifest

	// Verified is the number of entries whose contents matched the hash the database
	// recorded for them.
	Verified int

	// Unverifiable is the number of entries carried with no recorded hash to check
	// against — index.html and meta.json. Counted rather than ignored, so the total
	// adds up and nobody has to wonder which files were skipped.
	Unverifiable int

	// Absent names manifest entries the archive does not contain, and Corrupt names
	// entries whose bytes do not hash to what was recorded. Either one means this is
	// not a backup.
	Absent  []string
	Corrupt []string

	// Extra names entries in the archive that the manifest does not mention. Not a
	// failure: a file written while the tree was being walked is extra bytes, which
	// is the harmless direction. Reported because a large number of them means
	// something else is writing into the tree.
	Extra []string

	Bytes int64
}

// OK reports whether the archive is whole.
//
// Missing files that the *snapshot* referenced are a fault of the archive rather than
// of the copy — a prune during the backup — and are reported by the manifest itself
// rather than by this. So OK is about whether the file on disk is what was written.
func (r Report) OK() bool { return len(r.Absent) == 0 && len(r.Corrupt) == 0 }

// Summary is one line for a terminal.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d files verified against the hashes the archive recorded", r.Verified)
	if r.Unverifiable > 0 {
		fmt.Fprintf(&b, ", %d carried without one", r.Unverifiable)
	}
	if len(r.Extra) > 0 {
		fmt.Fprintf(&b, ", %d not in the manifest", len(r.Extra))
	}
	return b.String()
}

// maxEntryBytes bounds what a single entry in an archive may expand to.
//
// An archive is a file, and a file can come from anywhere — so a gzipped table that
// claims to be four kilobytes and expands forever is a way to fill a disk while
// somebody is trying to check a backup. The bound is deliberately far above anything
// real: this archive's whole database is 42MB compressed, and 64GiB in one table would
// be an archive nothing else here could hold either.
const maxEntryBytes = 64 << 30

// ErrNoManifest means the file is not one of these archives, or is truncated before
// its manifest.
//
// Worth its own error because it is the failure a truncated download produces, and
// "no manifest" is a far more useful thing to be told than a tar parse error at some
// offset.
var ErrNoManifest = errors.New("no manifest: this is not a Tomekeeper backup, or it is truncated")

// Verify reads an archive and checks it against its own manifest.
//
// **No database, no configuration, nothing but the file.** That is deliberate: the
// question "is this backup any good" has to be answerable on the machine the backup
// was copied to, months later, by somebody holding only the file and the binary. It
// streams once and hashes as it goes, so a 400MB archive costs one pass and no
// temporary space.
func Verify(r io.Reader) (*Report, error) {
	tr := tar.NewReader(r)

	found := make(map[string]string) // path inside blobs/ → hex sha256
	tables := make(map[string]string)
	var (
		manifest *Manifest
		report   Report
	)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		switch {
		case header.Name == manifestName:
			var m Manifest
			body, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading the manifest: %w", err)
			}
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("parsing the manifest: %w", err)
			}
			manifest = &m
			report.Bytes += int64(len(body))

		case strings.HasPrefix(header.Name, blobPrefix), strings.HasPrefix(header.Name, dbPrefix):
			sum := sha256.New()
			n, err := io.Copy(sum, io.LimitReader(tr, maxEntryBytes))
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", header.Name, err)
			}
			if n == maxEntryBytes {
				return nil, fmt.Errorf("%s is larger than %d bytes, which no archive this "+
					"program writes ever is", header.Name, int64(maxEntryBytes))
			}
			report.Bytes += n
			digest := hex.EncodeToString(sum.Sum(nil))
			if strings.HasPrefix(header.Name, blobPrefix) {
				found[strings.TrimPrefix(header.Name, blobPrefix)] = digest
			} else {
				tables[header.Name] = digest
			}

		default:
			// The README, and anything a later format adds. Counted, not judged.
			n, err := io.Copy(io.Discard, io.LimitReader(tr, maxEntryBytes))
			if err != nil {
				return nil, err
			}
			report.Bytes += n
		}
	}

	if manifest == nil {
		return nil, ErrNoManifest
	}
	report.Manifest = *manifest

	if manifest.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("this archive is format version %d and this build understands %d; "+
			"a newer tome wrote it", manifest.FormatVersion, FormatVersion)
	}

	for _, t := range manifest.Tables {
		got, ok := tables[t.Entry]
		switch {
		case !ok:
			report.Absent = append(report.Absent, t.Entry)
		case got != t.SHA256:
			report.Corrupt = append(report.Corrupt, t.Entry)
		default:
			report.Verified++
		}
		delete(tables, t.Entry)
	}
	for name := range tables {
		report.Extra = append(report.Extra, name)
	}

	for _, f := range manifest.Files {
		got, ok := found[f.Path]
		if !ok {
			report.Absent = append(report.Absent, f.Path)
			delete(found, f.Path)
			continue
		}
		delete(found, f.Path)

		if !f.Verifiable() {
			report.Unverifiable++
			continue
		}
		if got != f.SHA256 {
			report.Corrupt = append(report.Corrupt, f.Path)
			continue
		}
		report.Verified++
	}
	for p := range found {
		report.Extra = append(report.Extra, p)
	}

	sort.Strings(report.Absent)
	sort.Strings(report.Corrupt)
	sort.Strings(report.Extra)
	return &report, nil
}
