package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Reprocessing the whole archive from the interface.
//
// The last per-reader capability that lived only at a shell prompt. Per-domain
// reprocessing has been on the rules page since the rules page existed; the
// whole-archive form was `tome reextract` and nothing else, which was defensible
// while the bodies belonged to the household and is not once they belong to a reader.

// A reader's run moves their own bodies and leaves everybody else's alone.
//
// The assertion that matters is not that a page rendered — it is which article ids
// are waiting in the queue afterwards, and whose slot each job names.
func TestReprocessQueuesOnlyTheReadersOwnBodies(t *testing.T) {
	tr := setupTwoReadersFor(t)
	rd := signedInWithJobs(t, tr)
	ctx := t.Context()

	// Alice's own fork of a page, at an older extraction. This is the one that moves.
	mine := seedReextractable(t, tr, "https://example.com/posts/mine")
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: mine, Owner: store.Owned(tr.alice),
		ExtractorName: "domain_rule", ExtractorVersion: "2",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>Alice's own older extraction.</p>", Text: "Alice's own older extraction.",
		WordCount: 4,
	}); err != nil {
		t.Fatalf("InsertContent(alice's fork) = %v", err)
	}

	// A household body at the same older version, which a reader's run must not
	// touch: moving it would change what every other reader sees, from a control that
	// says nothing about doing so.
	household := seedReextractable(t, tr, "https://example.com/posts/household")

	// And an imported body of Alice's, which nothing bulk may ever re-extract: it may
	// be the only surviving copy of a page that is gone.
	imported, err := tr.store.ImportArticle(ctx, tr.alice, store.ImportParams{
		SourceName:   "wallabag",
		SourceID:     "reprocess-page-immutable",
		URLCanonical: "https://example.com/posts/imported",
		URLOriginal:  "https://example.com/posts/imported",
		ContentHTML:  "<p>The only copy.</p>",
		ContentText:  "The only copy.",
		WordCount:    3,
	})
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	clearExtractionJobs(t, tr)
	t.Cleanup(func() { clearExtractionJobs(t, tr) })

	rec := rd.do(http.MethodPost, "/reprocess", url.Values{
		"whose": {"mine"},
		"scope": {"all"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /reprocess = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Queued 1 article") {
		t.Errorf("the page does not report what was queued:\n%s", body)
	}
	// Whose bodies moved, said in the outcome. "Queued 1 article" reads identically
	// whether it moved one reader's private copy or what everybody reads.
	if body := rec.Body.String(); !strings.Contains(body, "your own bodies") {
		t.Errorf("the outcome does not say whose bodies moved:\n%s", body)
	}

	queued := queuedExtractionOwners(t, tr)
	if len(queued) != 1 {
		t.Fatalf("queued %d extraction jobs, want exactly 1: %v", len(queued), queued)
	}
	if owner, ok := queued[int64(mine)]; !ok {
		t.Errorf("Alice's own body was not queued; queued: %v", queued)
	} else if owner != int64(tr.alice) {
		t.Errorf("the job names user %d, want Alice (%d): a job in the wrong slot writes "+
			"the wrong body", owner, tr.alice)
	}
	if _, ok := queued[int64(household)]; ok {
		t.Error("a reader's reprocess queued the household's body, which every other reader reads")
	}
	if _, ok := queued[int64(imported.ArticleID)]; ok {
		t.Error("an imported, immutable body was queued for re-extraction")
	}
}

// Only an administrator may redo what everybody reads, and a refusal says so.
func TestReprocessRefusesTheHouseholdToAReader(t *testing.T) {
	tr := setupTwoReadersFor(t)
	rd := signedInWithJobs(t, tr)
	ctx := t.Context()

	// Something the household could reprocess, so that a refusal is a refusal rather
	// than an empty selection wearing one.
	seedReextractable(t, tr, "https://example.com/posts/everybodys")

	// The fixture's reader is the seeded operator account, which is an admin because
	// it is created from configuration with nobody else to promote it. Demoted here,
	// because the interesting case is the ordinary reader.
	if _, err := tr.pool.Exec(ctx,
		`UPDATE users SET role = $1 WHERE id = $2`, store.RoleReader, tr.alice); err != nil {
		t.Fatalf("demoting the fixture's reader: %v", err)
	}

	clearExtractionJobs(t, tr)
	t.Cleanup(func() { clearExtractionJobs(t, tr) })

	rec := rd.do(http.MethodPost, "/reprocess", url.Values{
		"whose": {"household"},
		"scope": {"all"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /reprocess (household, as a reader) = %d, want 403\n%s",
			rec.Code, rec.Body.String())
	}
	// Refused rather than quietly downgraded to their own bodies: a control that did
	// something smaller than it said would leave a reader believing the archive had
	// been reprocessed.
	if body := rec.Body.String(); !strings.Contains(body, "Nothing was queued") {
		t.Errorf("the refusal does not say that nothing happened:\n%s", body)
	}
	if queued := queuedExtractionOwners(t, tr); len(queued) != 0 {
		t.Errorf("a refused request queued work anyway: %v", queued)
	}
}

// The page counts what a reprocess would move before anybody presses anything.
func TestReprocessPageCountsWithoutAQueue(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	mine := seedReextractable(t, tr, "https://example.com/posts/countable")
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: mine, Owner: store.Owned(tr.alice),
		ExtractorName: "domain_rule", ExtractorVersion: "2",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>Alice's own older extraction.</p>", Text: "Alice's own older extraction.",
		WordCount: 4,
	}); err != nil {
		t.Fatalf("InsertContent(alice's fork) = %v", err)
	}

	body := rd.body("/reprocess")

	// A queue is needed to queue, not to count: this fixture has none, and the page
	// must still be able to say what a reprocess would move.
	if !strings.Contains(body, "no job queue configured") {
		t.Errorf("the page does not say that nothing can be queued here:\n%s", body)
	}
	if !strings.Contains(body, "tome reextract") {
		t.Errorf("the page does not name the command that does this instead:\n%s", body)
	}
	if !strings.Contains(body, "1 article") {
		t.Errorf("the page does not count the reader's own body:\n%s", body)
	}
	if !strings.Contains(body, "disabled") {
		t.Error("the buttons are live on an instance that cannot queue anything")
	}
}

// Holding no bodies of your own is the ordinary state, not a gap.
func TestReprocessPageSaysWhenThereIsNothingOfYours(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/reprocess")

	if !strings.Contains(body, "You hold no bodies of your own") {
		t.Errorf("the page does not explain that copy-on-write means there is nothing to redo:\n%s", body)
	}
	if strings.Contains(body, "Re-extract all 0 articles") {
		t.Error("the page offers a button over an empty count, which reads as something missing")
	}
}

// Both ways in, because the reader who wants this is standing in one of two places.
func TestReprocessIsReachableFromTheInterface(t *testing.T) {
	rd, _ := readingFixture(t)

	if body := rd.body("/settings"); !strings.Contains(body, `href="/reprocess"`) {
		t.Errorf("Settings does not link to the reprocess page:\n%s", body)
	}
	if body := rd.body("/domain-rules"); !strings.Contains(body, `href="/reprocess"`) {
		t.Errorf("the rules page does not offer the whole-archive form of its own button:\n%s", body)
	}
}

// signedInWithJobs is a signed-in reader on a server that can queue work.
//
// The same insert-only client `tome serve` builds, because a test that queued through
// some other path would not be testing the thing that runs.
func signedInWithJobs(t *testing.T, tr twoReadersHTTP) *reader {
	t.Helper()

	sessions, err := session.NewCookie([]byte("reprocess test secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	seedPassword(t, tr)

	jobClient, err := river.NewClient(riverpgxv5.New(tr.pool), &river.Config{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("river.NewClient() = %v", err)
	}

	srv := server.New(testConfig(), discardLogger(), server.Deps{
		Store: tr.store, Sessions: sessions, Jobs: jobClient,
	})
	rd := &reader{t: t, h: srv.Handler(), user: tr.alice}

	login := postLogin(t, rd.h, "tome", testPassword)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("signing in = %d", login.Code)
	}
	rd.jar = login.Result().Cookies()
	return rd
}

// queuedExtractionOwners is the queued extraction jobs as article id → owner slot.
//
// The owner is the half that a per-reader reprocess turns on: a job naming the wrong
// slot writes the wrong body, and it would do so silently.
func queuedExtractionOwners(t *testing.T, tr twoReadersHTTP) map[int64]int64 {
	t.Helper()

	rows, err := tr.pool.Query(t.Context(),
		`SELECT (args->>'article_id')::bigint, COALESCE((args->>'user_id')::bigint, 0)
		   FROM river_job WHERE kind = 'extract_article'`)
	if err != nil {
		t.Fatalf("reading the job queue: %v", err)
	}
	defer rows.Close()

	queued := make(map[int64]int64)
	for rows.Next() {
		var id, owner int64
		if err := rows.Scan(&id, &owner); err != nil {
			t.Fatalf("scanning a queued job: %v", err)
		}
		queued[id] = owner
	}
	return queued
}
