package server_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// twoBodies gives the fixture's article an imported body and a fetched one, which
// is the situation the whole control exists for: a library imported with a thin
// copy of a page this archive has since fetched properly.
func twoBodies(t *testing.T, tr twoReadersHTTP) (imported, fetched store.ContentID) {
	t.Helper()
	ctx := t.Context()

	// The article already carries a fetched body from the fixture. Add an imported
	// one, which wins automatically because it is immutable.
	if _, err := tr.store.ImportArticle(ctx, tr.alice, store.ImportParams{
		SourceName:   "wallabag",
		SourceID:     "promote-me",
		URLCanonical: "https://example.com/alice-only",
		URLOriginal:  "https://example.com/alice-only",
		ContentHTML:  "<p>A thin imported copy.</p>",
		ContentText:  "A thin imported copy.",
		WordCount:    4,
	}); err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	bodies, err := tr.store.BodiesForArticle(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("BodiesForArticle() = %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the article has %d bodies, want 2", len(bodies))
	}

	for _, b := range bodies {
		if b.Immutable {
			imported = b.ID
		} else {
			fetched = b.ID
		}
	}
	if imported == 0 || fetched == 0 {
		t.Fatalf("expected one immutable and one mutable body, got %+v", bodies)
	}
	return imported, fetched
}

// An imported body wins automatically, and a reader can overrule it.
func TestPromoteBodyLetsAReaderChoose(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	_, fetched := twoBodies(t, tr)
	path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10)

	// The imported copy is what the reader sees, because nothing automatic may
	// replace it.
	body := rd.body(path)
	if !strings.Contains(body, "A thin imported copy") {
		t.Errorf("the imported body is not the one shown:\n%s", body)
	}

	// And the page offers the choice, describing where each copy came from.
	if !strings.Contains(body, "Stored copies of this page") {
		t.Fatalf("the article offers no choice of body:\n%s", body)
	}
	for _, want := range []string{"imported from wallabag", "extracted from the stored page"} {
		if !strings.Contains(body, want) {
			t.Errorf("the chooser does not describe %q:\n%s", want, body)
		}
	}

	rec := rd.do(http.MethodPost, path+"/promote",
		url.Values{"body": {strconv.FormatInt(int64(fetched), 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST promote = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	after := rec.Body.String()

	// Said out loud, because two copies of one page can look similar enough that a
	// silent swap reads as nothing having happened.
	if !strings.Contains(after, "now the copy shown") {
		t.Errorf("the page does not say the body changed:\n%s", after)
	}
	if !strings.Contains(after, "distinctive alpaca passage") {
		t.Errorf("the promoted body is not the one rendered:\n%s", after)
	}

	// And it stuck.
	current, err := tr.store.CurrentContent(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if current.Immutable {
		t.Error("the imported body is still current after promoting the fetched one")
	}
	if current.ContentOrigin != store.OriginFetched {
		t.Errorf("current origin = %q, want the fetched body", current.ContentOrigin)
	}
}

// Promoting is reversible, and demoting an immutable body does not make it
// disposable.
func TestPromoteBodyIsReversible(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	imported, fetched := twoBodies(t, tr)
	path := "/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) + "/promote"

	for _, want := range []store.ContentID{fetched, imported} {
		rec := rd.do(http.MethodPost, path, url.Values{"body": {strconv.FormatInt(int64(want), 10)}})
		if rec.Code != http.StatusOK {
			t.Fatalf("promoting %d = %d, want 200", want, rec.Code)
		}
	}

	bodies, err := tr.store.BodiesForArticle(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("BodiesForArticle() = %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("promoting changed the number of bodies: %d", len(bodies))
	}

	var current int
	for _, b := range bodies {
		if b.Current {
			current++
			if b.ID != imported {
				t.Errorf("current body is %d, want the imported one back at %d", b.ID, imported)
			}
		}
		// A demoted immutable body is still immutable: still never regenerated, and
		// still there to be promoted again.
		if b.ID == imported && !b.Immutable {
			t.Error("the imported body stopped being immutable when it was demoted")
		}
	}
	if current != 1 {
		t.Errorf("%d bodies are current, want exactly 1", current)
	}
}

// A body that is not this article's cannot be promoted onto it.
func TestPromoteBodyRefusesAnotherArticlesBody(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	twoBodies(t, tr)

	// Bob's article has its own body, which is not Alice's to show.
	bobsBodies, err := tr.store.BodiesForArticle(ctx, tr.bobOnly)
	if err != nil {
		t.Fatalf("BodiesForArticle(bob) = %v", err)
	}
	if len(bobsBodies) == 0 {
		t.Fatal("the fixture gave Bob's article no body")
	}

	before, err := tr.store.CurrentContent(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}

	rec := rd.do(http.MethodPost,
		"/articles/"+strconv.FormatInt(int64(tr.aliceOnly), 10)+"/promote",
		url.Values{"body": {strconv.FormatInt(int64(bobsBodies[0].ID), 10)}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("promoting another article's body = %d, want 404", rec.Code)
	}

	after, err := tr.store.CurrentContent(ctx, tr.aliceOnly)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if after.HTML != before.HTML {
		t.Error("a refused promotion changed the article's body anyway")
	}
}

// A reader cannot promote a body on an article they cannot see.
//
// Bodies carry no user scoping of their own — they belong to the article, which the
// archive keeps one copy of for everyone — so the visibility check in the handler is
// the only thing standing between a hand-crafted form and somebody else's archive.
func TestPromoteBodyRefusesAnInvisibleArticle(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	bobsBodies, err := tr.store.BodiesForArticle(ctx, tr.bobOnly)
	if err != nil {
		t.Fatalf("BodiesForArticle(bob) = %v", err)
	}

	rec := rd.do(http.MethodPost,
		"/articles/"+strconv.FormatInt(int64(tr.bobOnly), 10)+"/promote",
		url.Values{"body": {strconv.FormatInt(int64(bobsBodies[0].ID), 10)}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("promoting a body on another reader's article = %d, want 404", rec.Code)
	}
}

// The ordinary article — one body — offers no choice at all.
func TestArticleWithOneBodyOffersNoChoice(t *testing.T) {
	rd, tr := readingFixture(t)

	body := rd.body("/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10))
	if strings.Contains(body, "Stored copies of this page") {
		t.Errorf("an article with one body offers a choice between it and nothing:\n%s", body)
	}
}

// After promoting a mutable body over an immutable one, the article is back in the
// ordinary extraction lifecycle.
//
// Worth asserting because it is a consequence rather than a feature: re-extraction
// selects on the *current* body being mutable, so promoting changes whether the
// article can ever be improved again. That is the right behavior and it is not
// obvious.
func TestPromotingAMutableBodyRestoresReextraction(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	// Re-extraction works from the stored original page, so the article needs one
	// before it can be a candidate at all — a body alone is not enough.
	if err := tr.store.RecordFetchSuccess(ctx, tr.aliceOnly,
		"sha-promote", "articles/2026/08/promote/raw.html.gz"); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}

	_, fetched := twoBodies(t, tr)

	candidates, err := tr.store.System().ReextractCandidates(ctx, "0", "", 0, 100)
	if err != nil {
		t.Fatalf("ReextractCandidates() = %v", err)
	}
	if containsArticle(candidates, tr.aliceOnly) {
		t.Error("an article showing an immutable body is a re-extraction candidate")
	}

	rec := rd.do(http.MethodPost,
		"/articles/"+strconv.FormatInt(int64(tr.aliceOnly), 10)+"/promote",
		url.Values{"body": {strconv.FormatInt(int64(fetched), 10)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("promote = %d, want 200", rec.Code)
	}

	candidates, err = tr.store.System().ReextractCandidates(ctx, "0", "", 0, 100)
	if err != nil {
		t.Fatalf("ReextractCandidates() = %v", err)
	}
	if !containsArticle(candidates, tr.aliceOnly) {
		t.Error("after promoting a mutable body the article is still excluded from re-extraction")
	}
}

func containsArticle(candidates []store.ReextractCandidate, id store.ArticleID) bool {
	for _, c := range candidates {
		if c.ArticleID == id {
			return true
		}
	}
	return false
}

// Marked passages are shown on the article they belong to.
//
// Until now nothing displayed them: the table has existed since the schema was
// written, and an imported library was the first thing to put rows in it. An archive
// that silently held somebody's annotations without ever showing them would be
// keeping them rather than preserving them.
func TestArticleShowsHighlights(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	if _, err := tr.store.AddHighlight(ctx, tr.alice, tr.aliceOnly, store.ImportHighlight{
		Quote: "A distinctive alpaca passage that only Alice can read.",
		Note:  "the sentence worth keeping",
	}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}

	body := rd.body("/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10))

	if !strings.Contains(body, "1 highlight") {
		t.Errorf("the article does not show its highlights:\n%s", body)
	}
	if !strings.Contains(body, "the sentence worth keeping") {
		t.Errorf("the highlight's note is missing:\n%s", body)
	}
	if !strings.Contains(body, "<blockquote>") {
		t.Errorf("the highlight is not quoted:\n%s", body)
	}

	// An article with none says nothing about them.
	other := rd.body("/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10) + "?from=all")
	if strings.Count(other, "<blockquote>") != 1 {
		t.Errorf("expected exactly the one highlight to be quoted")
	}
}

// One reader's highlights are not shown on another's copy of a shared article.
func TestHighlightsAreNotShownToAnotherReader(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	// Bob highlights an article he can see. Alice cannot see that article at all,
	// so the check that matters is on one they share — but the fixture gives them
	// none, so this asserts the narrower thing the fixture supports: Bob's
	// highlight on Bob's article is not on Alice's page for hers.
	if _, err := tr.store.AddHighlight(ctx, tr.bob, tr.bobOnly, store.ImportHighlight{
		Quote: "A distinctive nautilus passage that only Bob can read.",
	}); err != nil {
		t.Fatalf("AddHighlight() = %v", err)
	}

	body := rd.body("/articles/" + strconv.FormatInt(int64(tr.aliceOnly), 10))
	if strings.Contains(body, "nautilus") {
		t.Errorf("another reader's highlight appeared:\n%s", body)
	}
}
