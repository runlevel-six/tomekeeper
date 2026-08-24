package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runlevel-six/tomekeeper/internal/backup"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The drill §8 asks for, as a test: back an archive up, restore it into an empty
// database, and assert the two are the same archive.
//
// "A backup that has never been restored is not a backup" is the plan's own sentence,
// and until now the only restore this project had ever done was by hand, once, against
// one night's dump. This is the version that runs on every build.

// archiveFixture builds a small but complete archive: a reader, a feed, articles with
// bodies, an image shared by two of them, reading state, a tag, a highlight, and a
// domain rule. Small on purpose — what matters is that every table and both halves are
// represented, not that it is large.
type archiveFixture struct {
	pool   *pgxpool.Pool
	store  *store.Store
	user   store.UserID
	root   string
	assets map[string]string // path in the tree → contents
}

func newArchiveFixture(t *testing.T) archiveFixture {
	t.Helper()

	pool, s, userID := dbtest.SetupWithUser(t)
	ctx := t.Context()
	root := t.TempDir()

	feedID, _, err := s.UpsertFeed(ctx, userID, store.FeedParams{
		FeedURL: "https://example.com/feed.xml", Title: "Example Journal",
	})
	if err != nil {
		t.Fatalf("UpsertFeed() = %v", err)
	}

	fx := archiveFixture{pool: pool, store: s, user: userID, root: root,
		assets: make(map[string]string)}

	// Two articles, both with a stored page on disk and a body in the database.
	for i, slug := range []string{"first", "second"} {
		id, _, err := s.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: "https://example.com/" + slug,
			URLOriginal:  "https://example.com/" + slug,
			Title:        "Article " + slug,
		})
		if err != nil {
			t.Fatalf("UpsertArticle(%s) = %v", slug, err)
		}

		raw := "articles/2026/08/" + slug + "/raw.html.gz"
		fx.write(t, raw, "the stored page for "+slug)
		if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{
			SHA: sha(t, "the stored page for "+slug), Path: raw,
		}); err != nil {
			t.Fatalf("RecordFetchSuccess() = %v", err)
		}

		if _, err := s.InsertContent(ctx, store.ContentParams{
			ArticleID: id, Owner: store.Household(),
			ExtractorName: "trafilatura", ExtractorVersion: "7",
			ContentOrigin: store.OriginFetched,
			HTML:          "<p>Body " + slug + "</p>", Text: "Body " + slug, WordCount: 2,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}
		if _, err := s.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID: feedID, ArticleID: id, GUID: "guid-" + slug,
		}); err != nil {
			t.Fatalf("InsertFeedItem() = %v", err)
		}

		// A derived page, which the archive carries and cannot verify.
		fx.write(t, "articles/2026/08/"+slug+"/index.html", "<html>"+slug+"</html>")

		// One image, shared by both articles from the second one on: the dedupe that
		// makes the tree smaller than the sum of its articles, and the case a restore
		// has to get right.
		body := "not really a png"
		shaHex := sha(t, body)
		assetPath := "assets/sha256/" + shaHex[:2] + "/" + shaHex[2:4] + "/" + shaHex + ".png"
		if i == 0 {
			fx.write(t, assetPath, body)
			if _, err := s.UpsertAsset(ctx, store.Asset{
				SHA256: shaHex, MediaType: "image/png", ByteSize: int64(len(body)),
				Width: 1, Height: 1, FSPath: assetPath,
				SourceURL: "https://example.com/pic.png",
			}); err != nil {
				t.Fatalf("UpsertAsset() = %v", err)
			}
		}
		if err := s.LinkAsset(ctx, id, shaHex); err != nil {
			t.Fatalf("LinkAsset() = %v", err)
		}

		// Reading state, a tag, and a highlight, so the tables that hang off an
		// article are not empty in the dump.
		if _, err := s.SetRead(ctx, userID, id, true); err != nil {
			t.Fatalf("SetRead() = %v", err)
		}
		if i == 0 {
			if _, err := s.SetStarred(ctx, userID, id, true); err != nil {
				t.Fatalf("SetStarred() = %v", err)
			}
			if _, err := s.AddHighlight(ctx, userID, id,
				store.ImportHighlight{Quote: "Body " + slug}); err != nil {
				t.Fatalf("AddHighlight() = %v", err)
			}
		}
	}

	if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: "example.com", ContentSelector: "article", Notes: "round trip",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	return fx
}

func (fx archiveFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	full := filepath.Join(fx.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("creating %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o640); err != nil {
		t.Fatalf("writing %s: %v", rel, err)
	}
	fx.assets[rel] = body
}

// The whole point: an archive survives a round trip through a backup.
func TestBackupRestoreRoundTrip(t *testing.T) {
	fx := newArchiveFixture(t)
	ctx := t.Context()

	before := fingerprint(t, fx.pool)

	var buf bytes.Buffer
	result, err := backup.Write(ctx, fx.pool, &buf,
		backup.Options{BlobRoot: fx.root, Version: "test"})
	if err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if len(result.Manifest.Missing) != 0 {
		t.Errorf("the backup reports missing files on a quiet archive: %v", result.Manifest.Missing)
	}
	if result.Manifest.SchemaVersion == 0 {
		t.Error("the manifest records no schema version, so a restore cannot check it")
	}

	// It verifies, with no database in sight.
	report, err := backup.Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if !report.OK() {
		t.Fatalf("the archive it just wrote does not verify: absent=%v corrupt=%v",
			report.Absent, report.Corrupt)
	}
	if report.Verified == 0 {
		t.Error("nothing was verified, so this proves nothing")
	}

	// Restore into an emptied database and a new tree.
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	target := t.TempDir()

	restored, err := backup.Restore(ctx, fx.pool, path, backup.RestoreOptions{
		BlobRoot: target, Force: true,
	})
	if err != nil {
		t.Fatalf("Restore() = %v", err)
	}
	if restored.Rows == 0 || restored.Files == 0 {
		t.Fatalf("restore reported %d rows and %d files", restored.Rows, restored.Files)
	}

	// The database is the same archive.
	after := fingerprint(t, fx.pool)
	for table, want := range before {
		if after[table] != want {
			t.Errorf("%s: %d rows before, %d after", table, want, after[table])
		}
	}

	// And so is the tree, byte for byte. This is the assertion the hand drill made
	// once by comparing checksums, and the reason it is here is that the database
	// alone looks perfectly healthy without it.
	for rel, body := range fx.assets {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s was not restored: %v", rel, err)
			continue
		}
		if string(got) != body {
			t.Errorf("%s restored with different contents", rel)
		}
	}
}

// A truncated archive is refused, and says why.
//
// This is the failure the 2026-08-20 drill found: a documented copy that delivered 24%
// of the bytes and exited 0. A backup that cannot notice that is a copy.
func TestVerifyRefusesATruncatedArchive(t *testing.T) {
	fx := newArchiveFixture(t)

	var buf bytes.Buffer
	if _, err := backup.Write(t.Context(), fx.pool, &buf,
		backup.Options{BlobRoot: fx.root, Version: "test"}); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	full := buf.Bytes()

	// Cut off the last quarter: enough to lose the manifest, which is where a
	// truncation shows up first.
	if _, err := backup.Verify(bytes.NewReader(full[:len(full)*3/4])); err == nil {
		t.Error("a truncated archive verified, so a half-copied backup would look fine")
	}

	// And a corrupted body inside an otherwise complete archive: flip a byte in the
	// middle and the hash that the database recorded catches it.
	damaged := append([]byte(nil), full...)
	for i := len(damaged) / 2; i < len(damaged); i++ {
		if damaged[i] != 0 {
			damaged[i] ^= 0xFF
			break
		}
	}
	report, err := backup.Verify(bytes.NewReader(damaged))
	if err != nil {
		// A flipped byte can land in a tar header, which is a read error rather than
		// a hash mismatch. Either way it is caught, which is what matters.
		return
	}
	if report.OK() {
		t.Error("a corrupted archive verified as whole")
	}
}

// A restore refuses a database that already holds an archive.
func TestRestoreRefusesALiveArchive(t *testing.T) {
	fx := newArchiveFixture(t)
	ctx := t.Context()

	var buf bytes.Buffer
	if _, err := backup.Write(ctx, fx.pool, &buf,
		backup.Options{BlobRoot: fx.root, Version: "test"}); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}

	_, err := backup.Restore(ctx, fx.pool, path, backup.RestoreOptions{BlobRoot: t.TempDir()})
	if err == nil {
		t.Fatal("restoring over a populated database was allowed without --force")
	}
	// The refusal has to say what it found, or the reader cannot tell a wrong
	// argument from a broken archive.
	if !bytes.Contains([]byte(err.Error()), []byte("already holds")) {
		t.Errorf("the refusal does not say the database is not empty: %v", err)
	}
}

// A file the database references but the tree has lost is reported rather than hidden.
func TestBackupReportsAMissingFile(t *testing.T) {
	fx := newArchiveFixture(t)

	// A prune or an expiry between the snapshot and the walk looks exactly like this.
	var gone string
	for rel := range fx.assets {
		if filepath.Base(rel) == "raw.html.gz" {
			gone = rel
			break
		}
	}
	if gone == "" {
		t.Fatal("the fixture has no stored page to remove")
	}
	if err := os.Remove(filepath.Join(fx.root, filepath.FromSlash(gone))); err != nil {
		t.Fatalf("removing %s: %v", gone, err)
	}

	var buf bytes.Buffer
	result, err := backup.Write(t.Context(), fx.pool, &buf,
		backup.Options{BlobRoot: fx.root, Version: "test"})
	if err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if len(result.Manifest.Missing) != 1 || result.Manifest.Missing[0] != gone {
		t.Errorf("Missing = %v, want exactly %q", result.Manifest.Missing, gone)
	}

	// And the archive still verifies: the copy is faithful, the archive itself is
	// short a file, and those are different faults that must not be confused.
	report, err := backup.Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if !report.OK() {
		t.Errorf("a faithful copy of an archive missing a file did not verify: absent=%v", report.Absent)
	}
	if len(report.Manifest.Missing) != 1 {
		t.Errorf("the manifest's own record of the missing file did not survive: %v", report.Manifest.Missing)
	}
}

// fingerprint counts every table the backup carries, so a round trip can be compared.
func fingerprint(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()

	tables := []string{"users", "feeds", "articles", "assets", "article_content",
		"article_state", "article_assets", "article_tags", "domain_rules", "feed_items",
		"highlights", "import_records", "tags", "categories"}

	out := make(map[string]int64, len(tables))
	for _, table := range tables {
		var n int64
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func sha(t *testing.T, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// mkParams and recordRaw are the two store calls the scale test needs.
func mkParams(slug string) store.ArticleParams {
	return store.ArticleParams{
		URLCanonical: "https://example.com/" + slug,
		URLOriginal:  "https://example.com/" + slug,
		Title:        slug,
	}
}

func (fx archiveFixture) recordRaw(ctx context.Context, id store.ArticleID, rel, body string) error {
	sum := sha256.Sum256([]byte(body))
	return fx.store.RecordFetchSuccess(ctx, id, store.FetchedPage{
		SHA: hex.EncodeToString(sum[:]), Path: rel,
	})
}
