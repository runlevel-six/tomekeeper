package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The audit page: the archive's own maintenance report, scoped to one reader.
//
// `tome audit` needed a terminal and, on Kubernetes, permission to exec into a pod.
// Once a reader owns their own bodies, the person who most needs to know that one of
// them is a consent gate rather than an article is the one least likely to have
// either.

// The same title-and-wall pair the store tests use, so a finding here is a finding
// for the same measured reason rather than one this test invented.
const (
	auditTitle = "Understanding Distributed Consensus Algorithms"

	auditGoodBody = "Understanding distributed consensus algorithms takes practice, and this " +
		"piece works through several worked examples before arriving at the interesting part."

	auditWall = "This website uses cookies to improve your experience. Please accept or " +
		"decline before continuing, and review our privacy notice for the full detail."
)

// A reader's findings are about their own articles and nobody else's.
func TestAuditPageShowsOnlyYourOwnFindings(t *testing.T) {
	rd, tr := readingFixture(t)

	mine := seedAudited(t, tr, tr.alice, "https://mine.example.org/posts/consensus",
		auditTitle, auditWall)
	theirs := seedAudited(t, tr, tr.bob, "https://theirs.example.org/posts/consensus",
		auditTitle, auditWall)

	body := rd.body("/attention/audit")

	if !strings.Contains(body, "/articles/"+strconv.FormatInt(int64(mine), 10)) {
		t.Errorf("the reader's own suspect body is not reported:\n%s", body)
	}
	if strings.Contains(body, "/articles/"+strconv.FormatInt(int64(theirs), 10)) {
		t.Error("the page reports a finding about the other reader's article, which tells them it exists")
	}
	// The way out, on the row where the problem is named: a rule for that host, and
	// the explanation of what extraction did. Both were terminal-only work before.
	if !strings.Contains(body, "/domain-rules?edit=mine.example.org") {
		t.Errorf("the finding does not link to the rule form for its host:\n%s", body)
	}
	if !strings.Contains(body, "/articles/"+strconv.FormatInt(int64(mine), 10)+"/explain") {
		t.Errorf("the finding does not link to the explanation:\n%s", body)
	}
	// The error rate is stated before the findings rather than left to be discovered,
	// because a list of complaints with no stated precision reads as a list of faults.
	if !strings.Contains(body, "Expect false alarms") {
		t.Error("the page does not say that its lenses are deliberately imprecise")
	}
}

// A URL-titled article offers the remedy that can actually work on it.
func TestAuditPageOffersTheRemedyThatFits(t *testing.T) {
	rd, tr := readingFixture(t)

	// With a body: a stored page with a real title in it, so re-extraction fixes this
	// one. This fixture has no job queue, so the page must name the command rather
	// than offer a button that cannot do anything.
	withBody := seedAudited(t, tr, tr.alice, "https://mine.example.org/posts/bookmarked",
		"https://mine.example.org/posts/bookmarked", auditGoodBody)

	body := rd.body("/attention/audit")

	if !strings.Contains(body, "/articles/"+strconv.FormatInt(int64(withBody), 10)) {
		t.Fatalf("the URL-titled article is not reported:\n%s", body)
	}
	if !strings.Contains(body, "tome reextract") {
		t.Errorf("with no job queue the page does not name the command that does this:\n%s", body)
	}
	if strings.Contains(body, `href="/reprocess"`) {
		t.Error("the page offers a reprocess link on an instance that cannot queue one")
	}
}

// Nothing to look at is an answer, and the page still says what it looked for.
func TestAuditPageSaysWhatItLookedFor(t *testing.T) {
	rd, _ := readingFixture(t)

	body := rd.body("/attention/audit")

	if !strings.Contains(body, "Nothing to look at") {
		t.Errorf("an empty audit does not say so:\n%s", body)
	}
	// All three headings, whether they found anything or not: "none" only means
	// something if you can see what was looked for.
	for _, heading := range []string{
		"Bodies that say nothing their title mentions",
		"Bodies more than one article shares",
		"Titles that are not titles",
	} {
		if !strings.Contains(body, heading) {
			t.Errorf("the page does not name the %q lens:\n%s", heading, body)
		}
	}
}

// The attention queue is where somebody is standing when this question occurs to them.
func TestAttentionLinksToTheAudit(t *testing.T) {
	rd, _ := readingFixture(t)

	if body := rd.body("/attention"); !strings.Contains(body, `href="/attention/audit"`) {
		t.Errorf("the attention queue does not link to the audit:\n%s", body)
	}
	// And so is Settings, which is where a reader looks for the things that are
	// theirs.
	if body := rd.body("/settings"); !strings.Contains(body, `href="/attention/audit"`) {
		t.Errorf("Settings does not link to the audit:\n%s", body)
	}
}

// Fetching a page again from the audit comes back to the audit.
//
// The form posted `from` and nothing read it, so every button landed on the attention
// queue — which threw a reader off this page after the first of four fixes.
func TestRefetchFromTheAuditReturnsToTheAudit(t *testing.T) {
	tr := setupTwoReadersFor(t)
	rd := signedInWithJobs(t, tr)

	article := seedReextractable(t, tr, "https://mine.example.org/posts/refetch-me")
	if _, err := tr.store.SetKept(t.Context(), tr.alice, article, false); err != nil {
		t.Fatalf("SetKept() = %v", err)
	}
	clearExtractionJobs(t, tr)
	t.Cleanup(func() { clearExtractionJobs(t, tr) })

	rec := rd.do(http.MethodPost,
		"/articles/"+strconv.FormatInt(int64(article), 10)+"/refetch",
		url.Values{"from": {"audit"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST refetch = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/attention/audit" {
		t.Errorf("Location = %q, want the audit page the button was on", got)
	}

	// And an unrecognized value goes to the attention queue rather than wherever it
	// asked to go: a redirect built from what a form posted is an open redirect.
	rec = rd.do(http.MethodPost,
		"/articles/"+strconv.FormatInt(int64(article), 10)+"/refetch",
		url.Values{"from": {"https://evil.example.com/"}})
	if got := rec.Header().Get("Location"); got != "/attention" {
		t.Errorf("Location = %q, want the default rather than the submitted destination", got)
	}
}

// seedAudited stores an article one reader can see, with a household body, and
// returns it.
func seedAudited(t *testing.T, tr twoReadersHTTP, userID store.UserID,
	url, title, text string,
) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: url, URLOriginal: url, Title: title,
	})
	if err != nil {
		t.Fatalf("UpsertArticle(%s) = %v", url, err)
	}

	feedID := tr.aliceFeed
	if userID == tr.bob {
		feedID = tr.bobFeed
	}
	if _, err := tr.store.InsertFeedItem(ctx, userID, store.FeedItemParams{
		FeedID: feedID, ArticleID: id, GUID: fmt.Sprintf("audited-%d-%d", userID, id),
	}); err != nil {
		t.Fatalf("InsertFeedItem() = %v", err)
	}

	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: id, Owner: store.Household(),
		ExtractorName: "trafilatura", ExtractorVersion: "7",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>" + text + "</p>", Text: text, WordCount: 24,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	return id
}
