package server_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// libraryFixture is the same Wallabag export the exchange package's tests use.
//
// Shared deliberately: the web upload and the command line are two doors onto one
// importer, and a second fixture would be a second thing to keep in step.
const libraryFixture = "../exchange/testdata/imports/wallabag/library.json"

// libraryContents reads the shared fixture, which is the same export the exchange
// package's tests use — the web upload and the command line are two doors onto one
// importer, and a second fixture would be a second thing to keep in step.
func libraryContents(t *testing.T) string {
	t.Helper()

	contents, err := os.ReadFile(libraryFixture)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	return string(contents)
}

// The upload imports a library, and says what it did.
func TestUploadedLibraryIsImported(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.upload("/import", "library", "library.json", libraryContents(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /import = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"6 records",     // the fixture's size
		"from wallabag", // the format it was recognized as
		"Imported 6 articles",
		// The placeholder count, which is the number that changes what somebody
		// believes about their own library.
		"fetch-failure",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not report %q:\n%s", want, body)
		}
	}

	var articles int64
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM import_records WHERE user_id = $1`, tr.alice).Scan(&articles); err != nil {
		t.Fatalf("counting imports: %v", err)
	}
	if articles != 6 {
		t.Errorf("%d records were imported, want 6", articles)
	}

	// The imported articles are in the reading list, which is where a library
	// belongs and where somebody will look for it.
	saved := rd.body("/saved")
	if !strings.Contains(saved, "The ordinary case") {
		t.Errorf("an imported article is not in the reading list:\n%s", saved)
	}
}

// Report only means report only.
func TestUploadedLibraryReportOnlyWritesNothing(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.upload("/import", "library", "library.json", libraryContents(t), "report_only", "true")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /import = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "nothing was imported") {
		t.Errorf("the page does not say it wrote nothing:\n%s", body)
	}
	if !strings.Contains(body, "6 records") {
		t.Errorf("the report does not describe the file:\n%s", body)
	}
	if strings.Contains(body, "Imported 6 articles") {
		t.Errorf("a report-only upload claimed to have imported:\n%s", body)
	}

	var records int64
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM import_records WHERE user_id = $1`, tr.alice).Scan(&records); err != nil {
		t.Fatalf("counting imports: %v", err)
	}
	if records != 0 {
		t.Errorf("a report-only upload wrote %d import records, want 0", records)
	}
}

// Re-uploading the same file changes nothing, and says so.
func TestUploadedLibraryIsIdempotent(t *testing.T) {
	rd, tr := readingFixture(t)

	if rec := rd.upload("/import", "library", "library.json", libraryContents(t)); rec.Code != http.StatusOK {
		t.Fatalf("the first upload = %d", rec.Code)
	}

	rec := rd.upload("/import", "library", "library.json", libraryContents(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("the second upload = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "6 already imported") {
		t.Errorf("the second upload does not report the records as already imported:\n%s", body)
	}

	var articles int64
	if err := tr.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM articles`).Scan(&articles); err != nil {
		t.Fatalf("counting articles: %v", err)
	}
	// The two fixture articles from the reading fixture, plus the six imported.
	if articles != 8 {
		t.Errorf("%d articles after importing twice, want 8", articles)
	}
}

// A file that is not an export it can read is refused with the formats named.
func TestUploadedLibraryRefusesAnUnknownFormat(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.upload("/import", "library", "notes.txt", "This is prose, not an export.\n")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /import with prose = %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not a format this build can read") {
		t.Errorf("the refusal does not say what it reads:\n%s", body)
	}

	assertNothingImported(t, tr)
}

// A truncated export writes nothing at all, which is the property the two passes
// exist for.
func TestUploadedLibraryTruncatedWritesNothing(t *testing.T) {
	rd, tr := readingFixture(t)

	truncated, err := os.ReadFile("../exchange/testdata/imports/wallabag/cutbetweenrecords.json")
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	rec := rd.upload("/import", "library", "cut.json", string(truncated))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /import with a truncated export = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "nothing was imported") {
		t.Errorf("the page does not say nothing was imported:\n%s", body)
	}

	assertNothingImported(t, tr)
}

// No file chosen is a message, not a stack trace.
func TestUploadedLibraryWithNoFile(t *testing.T) {
	rd, _ := readingFixture(t)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("report_only", "true"); err != nil {
		t.Fatalf("building the form: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/import", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	for _, c := range rd.jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rd.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /import with no file = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No file was chosen") {
		t.Errorf("the page does not say the file is missing:\n%s", rec.Body.String())
	}
}

// The form is on the page, so somebody can find it without reading the docs.
func TestSavedPageOffersTheLibraryImport(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/saved")
	if !strings.Contains(body, `action="/import"`) {
		t.Errorf("the reading list offers no way to import a library:\n%s", body)
	}
	if !strings.Contains(body, "Settings → Export → JSON") {
		t.Errorf("the form does not say where the export comes from:\n%s", body)
	}
	if !strings.Contains(body, `name="report_only"`) {
		t.Errorf("the form offers no way to look before importing:\n%s", body)
	}
}

// Importing requires a session, like everything else that touches the archive.
func TestLibraryImportRequiresASession(t *testing.T) {
	rd, _ := readingFixture(t)

	unauthenticated := &reader{t: t, h: rd.h}
	rec := unauthenticated.upload("/import", "library", "library.json", libraryContents(t))
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /import without a session = %d, want a redirect or 401", rec.Code)
	}
}

func assertNothingImported(t *testing.T, tr twoReadersHTTP) {
	t.Helper()

	for _, q := range []struct{ label, sql string }{
		{"import records", `SELECT count(*) FROM import_records`},
		{"imported bodies", `SELECT count(*) FROM article_content WHERE content_origin LIKE 'import:%'`},
	} {
		var n int64
		if err := tr.pool.QueryRow(t.Context(), q.sql).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", q.label, err)
		}
		if n != 0 {
			t.Errorf("%s = %d after a refused upload, want 0", q.label, n)
		}
	}
}
