package server_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upload posts a file to an import form the way a browser would.
//
// Trailing name/value pairs become ordinary form fields alongside the file, which
// is what a checkbox next to a file input arrives as.
func (rd *reader) upload(path, field, filename, content string, fields ...string) *httptest.ResponseRecorder {
	rd.t.Helper()

	if len(fields)%2 != 0 {
		rd.t.Fatalf("upload got %d trailing values, want name/value pairs", len(fields))
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if field != "" {
		part, err := mw.CreateFormFile(field, filename)
		if err != nil {
			rd.t.Fatalf("CreateFormFile() = %v", err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			rd.t.Fatalf("writing the upload body: %v", err)
		}
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if err := mw.WriteField(fields[i], fields[i+1]); err != nil {
			rd.t.Fatalf("writing form field %s: %v", fields[i], err)
		}
	}
	if err := mw.Close(); err != nil {
		rd.t.Fatalf("closing the multipart writer: %v", err)
	}

	req := httptest.NewRequestWithContext(rd.t.Context(), http.MethodPost, path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range rd.jar {
		req.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	rd.h.ServeHTTP(rec, req)
	return rec
}

const sampleOPML = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>Subscriptions</title></head>
  <body>
    <outline text="Tech">
      <outline type="rss" text="Example Blog" xmlUrl="https://example.org/feed.xml" htmlUrl="https://example.org/"/>
      <outline type="rss" text="Another" xmlUrl="https://another.example/rss"/>
    </outline>
  </body>
</opml>`

func TestImportOPMLUploadSubscribes(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.upload("/feeds/import", "opml", "subscriptions.opml", sampleOPML)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /feeds/import = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Subscribed to 2 feeds") {
		t.Errorf("the page does not report what was imported:\n%s", body)
	}

	// The page rendered after an import must be the freshly counted feed list,
	// not a summary above a stale one.
	if !strings.Contains(body, "Example Blog") || !strings.Contains(body, "Another") {
		t.Errorf("the imported feeds are not listed on the page that reported them:\n%s", body)
	}

	// And they are really there, not just rendered from the request.
	if list := rd.body("/feeds"); !strings.Contains(list, "Example Blog") {
		t.Errorf("the feed list does not contain the imported feed:\n%s", list)
	}
}

// Re-importing is how someone recovers from an import that stopped halfway, so
// it has to be safe and it has to say plainly that nothing new happened.
func TestImportOPMLUploadIsIdempotent(t *testing.T) {
	rd, _ := readingFixture(t)

	if rec := rd.upload("/feeds/import", "opml", "subs.opml", sampleOPML); rec.Code != http.StatusOK {
		t.Fatalf("the first import = %d", rec.Code)
	}

	rec := rd.upload("/feeds/import", "opml", "subs.opml", sampleOPML)
	if rec.Code != http.StatusOK {
		t.Fatalf("the second import = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "already subscribed") {
		t.Errorf("re-importing does not report the feeds as already subscribed:\n%s", body)
	}
	if strings.Contains(body, "Subscribed to 2 feeds") {
		t.Errorf("re-importing claims to have added feeds again:\n%s", body)
	}
}

func TestImportOPMLUploadRejectsNonsense(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		content string
		status  int
		says    string
	}{
		{
			name:    "not xml at all",
			field:   "opml",
			content: "this is a text file, not an export",
			status:  http.StatusBadRequest,
			says:    "does not look like an OPML file",
		},
		{
			name:    "valid xml but not opml",
			field:   "opml",
			content: `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`,
			status:  http.StatusBadRequest,
			says:    "does not look like an OPML file",
		},
		{
			name:    "opml with no subscriptions",
			field:   "opml",
			content: `<?xml version="1.0"?><opml version="2.0"><head/><body></body></opml>`,
			status:  http.StatusBadRequest,
			says:    "no subscriptions",
		},
		{
			name:   "no file chosen",
			field:  "",
			status: http.StatusBadRequest,
			says:   "No file was chosen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd, _ := readingFixture(t)

			rec := rd.upload("/feeds/import", tt.field, "thing.opml", tt.content)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d\n%s", rec.Code, tt.status, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, tt.says) {
				t.Errorf("the page does not say %q, so the reader is not told what went wrong:\n%s", tt.says, body)
			}
		})
	}
}

// The body limit has to reject before the upload is buffered, so this asserts on
// the response rather than on how long it took: a 4MB cap that only applies after
// spooling to disk is not a cap.
func TestImportOPMLUploadRejectsHugeFiles(t *testing.T) {
	rd, _ := readingFixture(t)

	huge := strings.Repeat("x", 5<<20)
	rec := rd.upload("/feeds/import", "opml", "huge.opml", huge)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "larger than 4MB") {
		t.Errorf("the page does not explain the size limit:\n%s", body)
	}
}

// Signing out must take the upload with it. A route that mutates subscriptions
// is exactly the one worth checking is behind the same gate as everything else.
func TestImportOPMLRequiresSignIn(t *testing.T) {
	rd, _ := readingFixture(t)
	rd.jar = nil

	rec := rd.upload("/feeds/import", "opml", "subs.opml", sampleOPML)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated import = %d, want a redirect to sign in or 401\n%s",
			rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Subscribed to") {
		t.Error("an unauthenticated import reported success")
	}
}
