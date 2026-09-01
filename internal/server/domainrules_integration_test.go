package server_test

import (
	"context"
	"html"
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

// Writing a rule through the form stores what the CLI would store.
func TestDomainRuleFormSavesARule(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":   {"Example.COM"},
		"selector": {"div.relative > div.lightbox, .post-content"},
		"strip":    {".ad-wrapper\n\n  .related-stories  \n"},
		"notes":    {"body is split around the ads"},
		"rate":     {"0.5"},
		// The household's rule, which is what this form wrote when there was one
		// reader. A rate limit is a fetch setting and only the household may hold
		// one, so without this the save is refused — see the test below.
		"for_household": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Saved the rule") {
		t.Errorf("the page does not confirm the save:\n%s", body)
	}

	rule, err := tr.store.System().DomainRuleFor(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}

	// Lowercased, because a host is case-insensitive and a rule stored as
	// "Example.COM" would never match an article's host.
	if rule.Domain != "example.com" {
		t.Errorf("domain = %q, want it lowercased", rule.Domain)
	}
	if rule.ContentSelector != "div.relative > div.lightbox, .post-content" {
		t.Errorf("selector = %q", rule.ContentSelector)
	}
	// One selector per line, with the blank line and the stray spaces gone.
	if len(rule.StripSelectors) != 2 ||
		rule.StripSelectors[0] != ".ad-wrapper" || rule.StripSelectors[1] != ".related-stories" {
		t.Errorf("strip selectors = %q, want the two typed lines trimmed", rule.StripSelectors)
	}
	if rule.RateLimitRPS != 0.5 {
		t.Errorf("rate = %v, want 0.5", rule.RateLimitRPS)
	}

	// And the rule is in the table, where it can be reprocessed or removed.
	body := rd.body("/domain-rules")
	if !strings.Contains(body, "example.com") || !strings.Contains(body, ".post-content") {
		t.Errorf("the saved rule is not listed:\n%s", body)
	}
}

// A pasted URL is corrected rather than refused, because it is the obvious slip.
func TestDomainRuleFormAcceptsAPastedURL(t *testing.T) {
	rd, tr := readingFixture(t)

	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":   {"https://www.example.org/blog/2026/08/a-post?utm_source=x"},
		"selector": {"article.body"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	rule, err := tr.store.System().DomainRuleFor(t.Context(), "www.example.org")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}
	if rule.Domain != "www.example.org" {
		t.Errorf("domain = %q, want the host from the pasted URL", rule.Domain)
	}
}

// What cannot be a rule is refused with the form still filled in.
func TestDomainRuleFormRefusesWhatItCannotStore(t *testing.T) {
	rd, tr := readingFixture(t)

	for _, tc := range []struct {
		name   string
		form   url.Values
		expect string
	}{
		{
			name:   "not a domain",
			form:   url.Values{"domain": {"not a domain"}, "selector": {".body"}},
			expect: "does not look like a domain",
		},
		{
			name:   "a rate that is not a number",
			form:   url.Values{"domain": {"example.com"}, "selector": {".body"}, "rate": {"one per minute"}},
			expect: "must be a number",
		},
		{
			// The trap worth naming: a rule with nothing in it saves happily and does
			// nothing, and the only symptom is a site that still extracts badly.
			name:   "a rule that would do nothing",
			form:   url.Values{"domain": {"example.com"}},
			expect: "would do nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rd.do(http.MethodPost, "/domain-rules", tc.form)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /domain-rules = %d, want 400", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.expect) {
				t.Errorf("the complaint does not mention %q:\n%s", tc.expect, body)
			}
			// The form still holds what was typed, so it can be corrected rather
			// than retyped.
			if domain := tc.form.Get("domain"); domain != "" && !strings.Contains(body, `value="`+domain+`"`) {
				t.Errorf("the form lost what was typed:\n%s", body)
			}
		})
	}

	rules, err := tr.store.System().ListDomainRules(t.Context())
	if err != nil {
		t.Fatalf("ListDomainRules() = %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("a refused form stored %d rules: %+v", len(rules), rules)
	}
}

// Editing loads the stored rule, and only for an exact match.
func TestDomainRuleEditLoadsTheExactRule(t *testing.T) {
	rd, tr := readingFixture(t)

	if err := tr.store.System().UpsertDomainRule(t.Context(), store.DomainRule{
		Domain:          "example.com",
		ContentSelector: ".post-content",
		StripSelectors:  []string{".ad-wrapper"},
		Notes:           "the parent rule",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	body := rd.body("/domain-rules?edit=example.com")
	if !strings.Contains(body, `value=".post-content"`) {
		t.Errorf("editing did not load the selector:\n%s", body)
	}
	if !strings.Contains(body, ".ad-wrapper</textarea>") {
		t.Errorf("editing did not load the strip selectors:\n%s", body)
	}

	// A subdomain with no rule of its own must not open the parent's rule, or
	// saving would silently copy example.com's rule onto blog.example.com.
	body = rd.body("/domain-rules?edit=blog.example.com")
	if strings.Contains(body, `value=".post-content"`) {
		t.Errorf("editing a subdomain loaded the parent's rule:\n%s", body)
	}
	if !strings.Contains(body, `value="blog.example.com"`) {
		t.Errorf("the form does not offer to create a rule for the subdomain:\n%s", body)
	}
}

// Removing a rule removes it, and says what that means for what is already stored.
func TestDomainRuleDelete(t *testing.T) {
	rd, tr := readingFixture(t)

	if err := tr.store.System().UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: "example.com", ContentSelector: ".post-content",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	rec := rd.do(http.MethodPost, "/domain-rules/delete", url.Values{"domain": {"example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules/delete = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "keep the bodies it produced") {
		t.Errorf("the page does not explain what deleting leaves behind:\n%s", body)
	}

	rules, err := tr.store.System().ListDomainRules(t.Context())
	if err != nil {
		t.Fatalf("ListDomainRules() = %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("the rule survived deletion: %+v", rules)
	}

	// Removing one that is not there is a 404 rather than a cheerful success.
	if rec := rd.do(http.MethodPost, "/domain-rules/delete",
		url.Values{"domain": {"nothing.example"}}); rec.Code != http.StatusNotFound {
		t.Errorf("deleting a rule that does not exist = %d, want 404", rec.Code)
	}
}

// The article count beside a rule is what reprocessing it would select.
//
// The two numbers coming from one host expression is the point: a count that
// disagreed with what the button does would be worse than no count.
func TestDomainRuleCountsMatchWhatReprocessWouldSelect(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	if err := tr.store.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: "example.com", ContentSelector: ".post-content",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// Counted against a baseline rather than an absolute, because the fixture
	// already stores articles on this host.
	before, err := tr.store.System().ArticlesPerRuleDomain(ctx)
	if err != nil {
		t.Fatalf("ArticlesPerRuleDomain() = %v", err)
	}

	// Three articles: the host itself, a subdomain, and a host that merely contains
	// the domain as a substring — which must not count.
	for _, rawURL := range []string{
		"https://example.com/posts/one",
		"https://blog.example.com/posts/two",
		"https://notexample.com/posts/three",
	} {
		if _, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: rawURL, URLOriginal: rawURL,
		}); err != nil {
			t.Fatalf("UpsertArticle(%s) = %v", rawURL, err)
		}
	}

	counts, err := tr.store.System().ArticlesPerRuleDomain(ctx)
	if err != nil {
		t.Fatalf("ArticlesPerRuleDomain() = %v", err)
	}
	if added := counts["example.com"] - before["example.com"]; added != 2 {
		t.Errorf("adding three articles raised the count by %d, want 2 — the host and its "+
			"subdomain, but not notexample.com", added)
	}

	if body := rd.body("/domain-rules"); !strings.Contains(body, "example.com") {
		t.Errorf("the rules page does not list the rule:\n%s", body)
	}
}

// Without a job queue the reprocess control is absent and says so if posted to.
func TestReprocessWithoutAJobQueue(t *testing.T) {
	rd, tr := readingFixture(t)

	if err := tr.store.System().UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: "example.com", ContentSelector: ".post-content",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// The fixture's server has no job client, so the button is not drawn.
	if body := rd.body("/domain-rules"); strings.Contains(body, "/domain-rules/reprocess") {
		t.Error("an instance with no job queue offers a reprocess button")
	}

	rec := rd.do(http.MethodPost, "/domain-rules/reprocess", url.Values{"domain": {"example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules/reprocess = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no job queue") {
		t.Errorf("the page does not explain why reprocessing is unavailable:\n%s", body)
	}
	// And it names the command that does the same thing, rather than leaving the
	// reader stuck.
	if !strings.Contains(body, "tome reextract --target-version 0 --domain example.com") {
		t.Errorf("the page does not name the equivalent command:\n%s", body)
	}
}

// The page is reachable from the chrome, which is the whole reason it exists.
func TestDomainRulesAreLinkedFromEveryPage(t *testing.T) {
	rd, _ := readingFixture(t)

	if body := rd.body("/"); !strings.Contains(body, `href="/domain-rules"`) {
		t.Errorf("the navigation does not link to the rules page:\n%s", body)
	}
}

// Reprocess queues real jobs, which is the whole point of the control.
//
// The button exists because reprocessing is the second half of writing a rule, and
// until now it lived only at a shell prompt. So the assertion is not that a page
// rendered — it is that the worker has work waiting when the page comes back.
func TestReprocessQueuesExtractionJobs(t *testing.T) {
	tr := setupTwoReadersFor(t)
	ctx := t.Context()

	sessions, err := session.NewCookie([]byte("reprocess test secret"), session.DefaultTTL, true)
	if err != nil {
		t.Fatalf("NewCookie() = %v", err)
	}
	seedPassword(t, tr)

	// Insert-only, exactly as `tome serve` and `tome reextract` build it.
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

	if err := tr.store.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: "example.com", ContentSelector: ".post-content",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	// A candidate needs a stored raw page and a mutable current body — the two
	// things re-extraction works from.
	article := seedReextractable(t, tr, "https://example.com/posts/reprocess-me")

	// An imported body on the same host, which must never be selected: it may be
	// the only surviving copy of a page that is gone.
	imported, err := tr.store.ImportArticle(ctx, tr.alice, store.ImportParams{
		SourceName:   "wallabag",
		SourceID:     "reprocess-immutable",
		URLCanonical: "https://example.com/posts/imported",
		URLOriginal:  "https://example.com/posts/imported",
		ContentHTML:  "<p>The only copy.</p>",
		ContentText:  "The only copy.",
		WordCount:    3,
	})
	if err != nil {
		t.Fatalf("ImportArticle() = %v", err)
	}

	// The job queue is not truncated between packages the way the application
	// tables are, so leftovers from another suite would be counted here. Cleared
	// inside this test's own database lock.
	clearExtractionJobs(t, tr)

	// And cleared again afterwards, which matters more than it looks. This test
	// enqueues real jobs and there is no worker here to run them, so anything left
	// behind is picked up by the next package that starts one — naming an article id
	// that has since been truncated and reused, because dbtest restarts identity and
	// deliberately leaves River's tables alone. The symptom is a two-minute timeout
	// in an unrelated package's pipeline test, which is a long way from the cause.
	t.Cleanup(func() { clearExtractionJobs(t, tr) })

	rec := rd.do(http.MethodPost, "/domain-rules/reprocess", url.Values{"domain": {"example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules/reprocess = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Queued 1 article") {
		t.Errorf("the page does not report what was queued:\n%s", body)
	}

	queued := queuedExtractionArticles(t, tr)
	if len(queued) != 1 {
		t.Fatalf("queued %d extraction jobs, want exactly 1: %v", len(queued), queued)
	}
	if !queued[int64(article)] {
		t.Errorf("the article with a replaceable body was not queued; queued: %v", queued)
	}
	if queued[int64(imported.ArticleID)] {
		t.Error("an imported, immutable body was queued for re-extraction")
	}
}

// clearExtractionJobs empties the extraction queue, which no test here has a worker
// to drain.
func clearExtractionJobs(t *testing.T, tr twoReadersHTTP) {
	t.Helper()

	// Not t.Context(): cleanup runs after it is canceled.
	if _, err := tr.pool.Exec(context.Background(),
		`DELETE FROM river_job WHERE kind = 'extract_article'`); err != nil {
		t.Fatalf("clearing the job queue: %v", err)
	}
}

// seedReextractable stores an article that re-extraction can work from: a raw page
// on disk and a current, mutable body.
func seedReextractable(t *testing.T, tr twoReadersHTTP, rawURL string) store.ArticleID {
	t.Helper()
	ctx := t.Context()

	id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: rawURL, URLOriginal: rawURL, Title: "Reprocess me",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	if err := tr.store.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "sha-reprocess", Path: "articles/2026/08/reprocess/raw.html.gz"}); err != nil {
		t.Fatalf("RecordFetchSuccess() = %v", err)
	}
	if _, err := tr.store.InsertContent(ctx, store.ContentParams{
		ArticleID: id, ExtractorName: "trafilatura", ExtractorVersion: "2",
		ContentOrigin: store.OriginFetched,
		HTML:          "<p>An older extraction.</p>", Text: "An older extraction.", WordCount: 3,
	}); err != nil {
		t.Fatalf("InsertContent() = %v", err)
	}
	return id
}

// queuedExtractionArticles is the set of article ids with an extraction job waiting.
func queuedExtractionArticles(t *testing.T, tr twoReadersHTTP) map[int64]bool {
	t.Helper()

	rows, err := tr.pool.Query(t.Context(),
		`SELECT (args->>'article_id')::bigint FROM river_job WHERE kind = 'extract_article'`)
	if err != nil {
		t.Fatalf("reading the job queue: %v", err)
	}
	defer rows.Close()

	queued := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning a queued job: %v", err)
		}
		queued[id] = true
	}
	return queued
}

// The attention queue links to the rule form for the site that failed.
//
// This is the whole workflow closing: a badly-extracted article is discovered
// here, and the fix used to begin by leaving the browser.
func TestAttentionLinksToTheRuleForm(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	id, _, err := tr.store.UpsertArticle(ctx, store.ArticleParams{
		URLCanonical: "https://troublesome.example.org/posts/paywalled",
		URLOriginal:  "https://troublesome.example.org/posts/paywalled",
		Title:        "A page that would not extract",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	// Visible to the reader, and failed: both are needed for it to appear here.
	if _, err := tr.store.SetKept(ctx, tr.alice, id, false); err != nil {
		t.Fatalf("SetKept() = %v", err)
	}
	if _, err := tr.pool.Exec(ctx,
		`INSERT INTO article_state (user_id, article_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, tr.alice, id); err != nil {
		t.Fatalf("making the article visible: %v", err)
	}
	if err := tr.store.RecordFetchFailure(ctx, id, store.FetchFailed, "HTTP 403"); err != nil {
		t.Fatalf("RecordFetchFailure() = %v", err)
	}

	body := rd.body("/attention")
	if !strings.Contains(body, "A page that would not extract") {
		t.Fatalf("the failed article is not in the queue:\n%s", body)
	}
	if !strings.Contains(body, "/domain-rules?edit=troublesome.example.org") {
		t.Errorf("the row does not link to a rule for its host:\n%s", body)
	}
}

// A reader's own rule may set selectors and nothing else, and the refusal says why
// rather than reporting a constraint.
func TestAReadersRuleRefusesFetchSettings(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":   {"example.com"},
		"selector": {"main"},
		"rate":     {"0.5"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST a reader rule with a rate = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if body := rec.Body.String(); !strings.Contains(body, "fetched once") {
		t.Errorf("the refusal does not explain why:\n%s", body)
	}
}

// A reader's own rule is saved against them, leaving the household's alone.
func TestAReadersRuleIsSavedAgainstThem(t *testing.T) {
	rd, tr := readingFixture(t)

	// The household's rule first, so there is something for the reader's to not
	// disturb.
	if err := tr.store.System().UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: "example.com", ContentSelector: "main.house",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}

	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":   {"example.com"},
		"selector": {"main.mine"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "for <strong>you</strong>") {
		t.Errorf("the page does not say whose rule was saved:\n%s", body)
	}

	// The household's is untouched.
	household, err := tr.store.System().DomainRuleFor(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}
	if household.ContentSelector != "main.house" {
		t.Errorf("the household selector is now %q; a reader's rule overwrote it",
			household.ContentSelector)
	}

	// And the reader gets their own.
	mine, err := tr.store.System().EffectiveRuleFor(t.Context(), tr.alice, "example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor() = %v", err)
	}
	if mine.ContentSelector != "main.mine" || !mine.FromReader {
		t.Errorf("the reader's effective selector = %q (own: %v), want their own",
			mine.ContentSelector, mine.FromReader)
	}
}

// The User-Agent is the third fetch setting, and the one nobody could reach: it was
// in the schema and in the store's types from the beginning, settable only by
// somebody willing to write SQL against the archive's own database.
func TestDomainRuleFormSavesAUserAgent(t *testing.T) {
	rd, tr := readingFixture(t)

	const agent = "Mozilla/5.0 (compatible; tomekeeper; +https://example.com/tomekeeper)"

	// A user agent on its own, with no selector and nothing to strip. That has to be
	// a rule the form accepts: a site that refuses the default identity is usually a
	// site whose markup the extractors handle perfectly well.
	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":        {"picky.example"},
		"user_agent":    {agent},
		"notes":         {"refuses anything that does not look like a browser"},
		"for_household": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /domain-rules = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	rule, err := tr.store.System().DomainRuleFor(t.Context(), "picky.example")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}
	if rule.UserAgent != agent {
		t.Errorf("user agent = %q, want %q", rule.UserAgent, agent)
	}

	// The table says one is set without printing the whole string into the column.
	if body := rd.body("/domain-rules"); !strings.Contains(body, "custom user agent") {
		t.Errorf("the table does not show that a user agent is set:\n%s", body)
	}

	// And the edit form loads it back, so changing something else does not silently
	// clear it — the trap the CLI's replace-in-place upsert had.
	//
	// Unescaped before comparing, because html/template writes the '+' in a contact
	// URL as &#43; inside an attribute. The browser decodes it and the field holds
	// the right string; an assertion against the raw text would fail on correct
	// output, which is exactly what it did when this was written.
	edit := html.UnescapeString(rd.body("/domain-rules?edit=picky.example"))
	if !strings.Contains(edit, agent) {
		t.Errorf("the edit form does not load the stored user agent:\n%s", edit)
	}
}

// A reader may not set one, for the same reason they may not ask for a browser:
// the page is fetched once, so the identity it is fetched under is everybody's.
func TestAReadersRuleRefusesAUserAgent(t *testing.T) {
	rd, _ := readingFixture(t)

	rec := rd.do(http.MethodPost, "/domain-rules", url.Values{
		"domain":     {"picky.example"},
		"selector":   {"main"},
		"user_agent": {"Mozilla/5.0 (compatible; tomekeeper; +https://example.com)"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST a reader rule with a user agent = %d, want %d",
			rec.Code, http.StatusBadRequest)
	}
	if body := rec.Body.String(); !strings.Contains(body, "fetched once") {
		t.Errorf("the refusal does not explain why:\n%s", body)
	}
}
