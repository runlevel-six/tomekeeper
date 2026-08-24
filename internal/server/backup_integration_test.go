package server_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/backup"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
)

// The backup download: the household's bytes, so administrators only.
//
// It is not the export. `/export` is one reader's articles, scoped by what they can
// see and importable into another reader; this is every reader's articles plus the
// fetched pages no account owns. A reader must not be able to reach it, and — like
// every other admin route here — must be told 404 rather than 403, because a 403
// confirms the route is there.
func TestBackupDownloadIsAdminOnly(t *testing.T) {
	tr := setupTwoReadersFor(t)
	root := t.TempDir()

	// One file in the tree, so the archive has something to carry.
	dir := filepath.Join(root, "articles", "2026", "08", "one")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating the tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>hi</html>"), 0o640); err != nil {
		t.Fatalf("writing a file: %v", err)
	}

	sessions, err := session.NewCookie([]byte("backup test secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	seedPassword(t, tr)
	srv := server.New(testConfig(), discardLogger(), server.Deps{
		Store: tr.store, Sessions: sessions, BlobRoot: root,
	})
	rd := &reader{t: t, h: srv.Handler(), user: tr.alice}
	login := postLogin(t, rd.h, "tome", testPassword)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("signing in = %d", login.Code)
	}
	rd.jar = login.Result().Cookies()

	// The seeded account is the operator and is an administrator.
	rec := rd.get("/backup")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /backup as an administrator = %d\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-tar" {
		t.Errorf("Content-Type = %q, want application/x-tar", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}
	// A snapshot of a changing archive must never be cached as though it were a file.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the download is empty")
	}
	// And it is really an archive: verify what the browser would have saved, with no
	// database in the loop — the same check the command offers.
	report, err := backup.Verify(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the downloaded archive does not verify: %v", err)
	}
	if !report.OK() {
		t.Errorf("the downloaded archive is not whole: absent=%v corrupt=%v",
			report.Absent, report.Corrupt)
	}

	// Demoted, and the route disappears rather than refusing.
	if _, err := tr.pool.Exec(t.Context(),
		`UPDATE users SET role = 'reader' WHERE id = $1`, tr.alice); err != nil {
		t.Fatalf("demoting: %v", err)
	}
	if rec := rd.get("/backup"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /backup as a reader = %d, want 404 — a 403 would confirm the route exists", rec.Code)
	}
}

// With no archive tree there is nothing to copy, and the route says so instead of
// offering an archive with no files in it.
func TestBackupDownloadWithoutATree(t *testing.T) {
	rd, _ := readingFixture(t)

	// readingFixture wires no BlobRoot, which is the case under test.
	if rec := rd.get("/backup"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /backup with no tree = %d, want 503", rec.Code)
	}
}
