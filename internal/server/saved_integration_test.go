package server_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

func storeSavedOnly() store.StreamQuery { return store.StreamQuery{SavedOnly: true} }

func TestSaveAPageArchivesIt(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/save", url.Values{"url": {"https://example.net/a-long-read"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /save = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Saved.") {
		t.Errorf("the page does not confirm the save:\n%s", body)
	}
	// Nothing has fetched it yet, so the reader must be told that rather than
	// being handed a link to an empty article.
	if !strings.Contains(body, "Queued for fetching") {
		t.Errorf("the page does not say the fetch is queued:\n%s", body)
	}

	// A separate GET, so this cannot be satisfied by the confirmation banner that
	// the POST response also contains.
	list := rd.body("/saved")
	if !strings.Contains(list, "example.net/a-long-read") {
		t.Errorf("the saved page is not in the reading list:\n%s", list)
	}
	// An article nothing has fetched yet has no title, and a list of rows all
	// reading "(untitled)" would be unusable.
	if strings.Contains(list, "(untitled)") {
		t.Errorf("an untitled saved page renders with no way to identify it:\n%s", list)
	}
	if !strings.Contains(list, ">queued</span>") {
		t.Errorf("a queued page is not distinguished from one that failed to extract:\n%s", list)
	}
}

// A saved URL the archive already holds must deduplicate onto the existing
// article and be readable at once. This is the property that makes saving cheap.
func TestSavingAnArticleTheArchiveAlreadyHas(t *testing.T) {
	rd, tr := readingFixture(t)

	// aliceOnly already exists with a body, from the fixture.
	existing, err := tr.store.ArticleForUser(t.Context(), tr.alice, tr.aliceOnly)
	if err != nil {
		t.Fatalf("ArticleForUser() = %v", err)
	}

	rec := rd.do(http.MethodPost, "/save", url.Values{"url": {existing.Article.URLCanonical}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /save = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Read it now") {
		t.Errorf("saving a page already in the archive does not offer to read it:\n%s", body)
	}
	if strings.Contains(body, "Queued for fetching") {
		t.Errorf("a page with a body was queued for fetching anyway:\n%s", body)
	}
}

func TestSavingTheSameURLTwiceIsIdempotent(t *testing.T) {
	rd, _ := readingFixture(t)
	const target = "https://example.net/saved-twice"

	if rec := rd.do(http.MethodPost, "/save", url.Values{"url": {target}}); rec.Code != http.StatusOK {
		t.Fatalf("the first save = %d", rec.Code)
	}

	rec := rd.do(http.MethodPost, "/save", url.Values{"url": {target}})
	if rec.Code != http.StatusOK {
		t.Fatalf("the second save = %d\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Already saved") {
		t.Errorf("re-saving does not report it was already saved:\n%s", body)
	}

	// And it appears once, not twice.
	if n := strings.Count(rd.body("/saved"), "saved-twice"); n != 1 {
		t.Errorf("the saved page appears %d times in the reading list, want 1", n)
	}
}

// A URL pasted from an address bar routinely has no scheme. Rejecting that would
// be technically correct and practically useless.
func TestSavingAcceptsASchemelessURL(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/save", url.Values{"url": {"example.net/no-scheme"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /save = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "https://example.net/no-scheme") {
		t.Errorf("the schemeless URL was not resolved to https:\n%s", body)
	}
}

func TestSavingRejectsWhatItCannotArchive(t *testing.T) {
	for _, bad := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"not a url at all with spaces",
	} {
		t.Run(bad, func(t *testing.T) {
			rd, _ := readingFixture(t)

			rec := rd.do(http.MethodPost, "/save", url.Values{"url": {bad}})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /save %q = %d, want 400\n%s", bad, rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); strings.Contains(body, "Saved.") {
				t.Errorf("%q was reported as saved", bad)
			}
		})
	}
}

// The reading list is user-scoped like everything else: one reader's saved pages
// must not appear in another's.
func TestSavedPagesAreNotVisibleToOtherReaders(t *testing.T) {
	rd, tr := readingFixture(t)

	if rec := rd.do(http.MethodPost, "/save",
		url.Values{"url": {"https://example.net/alices-private-save"}}); rec.Code != http.StatusOK {
		t.Fatalf("saving = %d", rec.Code)
	}

	items, err := tr.store.Stream(t.Context(), tr.bob, storeSavedOnly())
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	for _, it := range items {
		if strings.Contains(it.URLCanonical, "alices-private-save") {
			t.Fatalf("bob can see alice's saved page: %s", it.URLCanonical)
		}
	}
}

// The window between extraction and asset localization is invisible and looks
// like a bug: the body still points at the origin site, the content security
// policy blocks every remote image, and the reader gets correctly-sized blank
// rectangles. Saying so is the difference between "still working on it" and
// "this is broken".
func TestPendingImagesAreExplained(t *testing.T) {
	rd, tr := readingFixture(t)

	if _, err := tr.pool.Exec(t.Context(),
		`UPDATE articles SET assets_status = 'pending' WHERE id = $1`, tr.aliceOnly); err != nil {
		t.Fatalf("setting assets_status: %v", err)
	}

	body := rd.body("/articles/" + itoa(int64(tr.aliceOnly)))
	if !strings.Contains(body, "have not been archived yet") {
		t.Errorf("an article whose images are still queued does not say so:\n%s", body)
	}
}

func TestPartiallyArchivedImagesAreExplained(t *testing.T) {
	rd, tr := readingFixture(t)

	if _, err := tr.pool.Exec(t.Context(),
		`UPDATE articles SET assets_status = 'partial' WHERE id = $1`, tr.aliceOnly); err != nil {
		t.Fatalf("setting assets_status: %v", err)
	}

	body := rd.body("/articles/" + itoa(int64(tr.aliceOnly)))
	if !strings.Contains(body, "could not be archived") {
		t.Errorf("an article with unarchivable images does not say so:\n%s", body)
	}
	if !strings.Contains(body, "never loaded from the original site") {
		t.Errorf("the reader is not told why the gap is not filled by hotlinking:\n%s", body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// "Is this worth opening yet" is the question the badge answers. Getting it
// wrong in the optimistic direction is the expensive one: an article marked
// complete that renders as blank rectangles is why this exists.
func TestStreamBadgeReportsArchivingState(t *testing.T) {
	tests := []struct {
		name         string
		fetchStatus  string
		assetsStatus string
		dropBody     bool
		want         string
		notWant      string
	}{
		{name: "fully archived", fetchStatus: "ok", assetsStatus: "ok", want: "complete"},
		{name: "images still queued", fetchStatus: "ok", assetsStatus: "pending",
			want: "images pending", notWant: "complete"},
		{name: "images incomplete", fetchStatus: "ok", assetsStatus: "partial",
			want: "images incomplete", notWant: "complete"},
		{name: "not fetched yet", fetchStatus: "pending", assetsStatus: "pending",
			dropBody: true, want: "queued", notWant: "complete"},
		{name: "fetch failed", fetchStatus: "failed", assetsStatus: "none",
			dropBody: true, want: "no body", notWant: "complete"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd, tr := readingFixture(t)

			if _, err := tr.pool.Exec(t.Context(),
				`UPDATE articles SET fetch_status = $2, assets_status = $3 WHERE id = $1`,
				tr.aliceOnly, tt.fetchStatus, tt.assetsStatus); err != nil {
				t.Fatalf("setting article state: %v", err)
			}
			if tt.dropBody {
				if _, err := tr.pool.Exec(t.Context(),
					`DELETE FROM article_content WHERE article_id = $1`, tr.aliceOnly); err != nil {
					t.Fatalf("removing the body: %v", err)
				}
			}

			// Matched with the tag delimiters, because "images incomplete"
			// contains "complete" and a bare substring check passes for exactly
			// the case this is meant to catch.
			body := rd.body("/all")
			if want := ">" + tt.want + "</span>"; !strings.Contains(body, want) {
				t.Errorf("the stream does not show the %q badge:\n%s", tt.want, body)
			}
			if tt.notWant != "" {
				if bad := ">" + tt.notWant + "</span>"; strings.Contains(body, bad) {
					t.Errorf("the stream shows the %q badge for an article that is not:\n%s", tt.notWant, body)
				}
			}
		})
	}
}
