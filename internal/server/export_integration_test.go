package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The download is the archive, and it is what the importer reads back.
func TestExportDownload(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.get("/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /export = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	// Offered as a file rather than rendered, with a dated name somebody can find
	// afterwards.
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, `attachment; filename="tomekeeper-`) {
		t.Errorf("Content-Disposition = %q, want a dated attachment", disposition)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	// A snapshot of something that changes: a cached one masquerading as a backup
	// is worse than none.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var records []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &records); err != nil {
		t.Fatalf("the download is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	// The fixture's reader can see one article; the other reader's is not hers.
	if len(records) != 1 {
		t.Fatalf("the download holds %d records, want 1", len(records))
	}
	if url, _ := records[0]["url"].(string); !strings.Contains(url, "alice-only") {
		t.Errorf("the download holds the wrong article: %v", records[0]["url"])
	}
	if strings.Contains(rec.Body.String(), "nautilus") {
		t.Error("the download carries the other reader's article")
	}
}

// The settings page offers the download, and says what it does not contain.
func TestSettingsOffersTheExport(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/settings")

	if !strings.Contains(body, `href="/export"`) {
		t.Errorf("the settings page offers no export:\n%s", body)
	}
	if !strings.Contains(body, "1 article") {
		t.Errorf("the control does not say how much it will download:\n%s", body)
	}
	// The sentence that stops somebody believing this file is the whole archive.
	if !strings.Contains(body, "referenced by path") {
		t.Errorf("the page does not say images are not included:\n%s", body)
	}
}

// Exporting requires a session, like everything else that touches the archive.
func TestExportRequiresASession(t *testing.T) {
	rd, _ := readingFixture(t)

	anonymous := &reader{t: t, h: rd.h}
	rec := anonymous.get("/export")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /export without a session = %d, want a redirect or 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice-only") {
		t.Error("the archive was served to a request with no session")
	}
}

// What the download holds is what an import puts back.
//
// The round trip itself is tested in the exchange package against the store; this
// asserts the smaller thing that only the HTTP layer can get wrong — that the bytes
// a browser receives are the bytes the importer reads.
func TestExportDownloadIsImportable(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.get("/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /export = %d", rec.Code)
	}

	upload := rd.upload("/import", "library", "archive.json", rec.Body.String(), "report_only", "true")
	if upload.Code != http.StatusOK {
		t.Fatalf("re-importing the download = %d, want 200\n%s", upload.Code, upload.Body.String())
	}
	body := upload.Body.String()
	if !strings.Contains(body, "1 record") {
		t.Errorf("the download was not recognized as an importable archive:\n%s", body)
	}
	// Already here, since it is this archive's own export: the report should say so
	// rather than offering to add it again.
	if !strings.Contains(body, "already in the archive") && !strings.Contains(body, "1 already imported") {
		t.Errorf("re-importing this archive's own export does not recognize it:\n%s", body)
	}
}
