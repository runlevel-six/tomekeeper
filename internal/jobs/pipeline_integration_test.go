package jobs_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/runlevel-six/tomekeeper/internal/archive"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/render"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// These require a live PostgreSQL and skip without TOME_TEST_DATABASE_URL.

const articlePage = `<!DOCTYPE html>
<html lang="en">
<head><title>The Archived Article</title>
  <meta property="og:site_name" content="Example Journal">
  <meta name="author" content="Dana Okonkwo">
</head>
<body>
  <nav>Home About Subscribe now for unlimited access to everything</nav>
  <article>
    <h1>The Archived Article</h1>
    <p>An archive is only worth having if what it holds can still be read years
    later, which means the bytes have to be kept rather than a link to them. A
    link is a promise that someone else will keep the bytes, and that promise
    is broken often enough to be worth not relying on.</p>
    <p>Keeping the original fetch is what makes every later improvement to
    extraction apply to the whole archive rather than only to what arrives
    afterwards. Without it, a better extractor helps only the future.</p>
    <p>That is the entire argument for storing raw pages, and it is why this
    costs disk space that a feed reader would not spend.</p>
  </article>
  <footer>Copyright 2026. All rights reserved. Privacy policy.</footer>
</body>
</html>`

// runPipeline starts a worker, runs fn, and waits for the queue to drain.
func runPipeline(t *testing.T, s *store.Store, blobs blob.Store, client *httpclient.Client,
	fn func(context.Context, *river.Client[pgx.Tx]),
) {
	t.Helper()

	// No renderer, which is the deployment nearly everybody has and the one these tests
	// were written against. runPipelineWith is for the handoff cases that need one.
	runPipelineWith(t, s, blobs, client, nil, fn)
}

// runPipelineWith is runPipeline with a renderer.
func runPipelineWith(t *testing.T, s *store.Store, blobs blob.Store, client *httpclient.Client,
	renderer *render.Renderer, fn func(context.Context, *river.Client[pgx.Tx]),
) {
	t.Helper()

	pool := s.Pool()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Start from an empty queue, and the reason is worth stating because the failure
	// it prevents points nowhere near here.
	//
	// dbtest truncates the application tables with RESTART IDENTITY, so every test
	// begins with article id 1 again — and it deliberately leaves River's tables
	// alone, because they are not part of this schema. A job queued by another
	// package therefore names an article id that now belongs to a different article
	// entirely, and this is the only package that starts a worker to run one.
	//
	// Observed as exactly that: a stray extract_article job ran against a
	// freshly-created article that had not been fetched yet, found no stored page,
	// marked it failed, and left the pipeline test waiting out its whole budget for
	// an image that was never going to be localized.
	if _, err := pool.Exec(t.Context(), `DELETE FROM river_job`); err != nil {
		t.Fatalf("clearing the job queue before starting a worker: %v", err)
	}

	riverClient, err := jobs.NewWorkerClient(jobs.Deps{
		Pool:        pool,
		Store:       s,
		Poller:      feed.NewPoller(s, client, feed.DefaultIntervalPolicy(), 20, log),
		Client:      client,
		Blobs:       blobs,
		Extractor:   extract.New(),
		Log:         log,
		Concurrency: 2,
		Renderer:    renderer,
	})
	if err != nil {
		t.Fatalf("NewWorkerClient() = %v", err)
	}

	ctx := t.Context()
	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	})

	fn(ctx, riverClient)
}

// waitForBudget is how long a postcondition has to become true.
//
// Generous on purpose. The longest of these waits on ten articles' images being
// transcoded, which is real AVIF encoding at `TOME_IMAGE_CONCURRENCY` of one —
// it takes around 17 seconds alone on a developer's machine, and the whole suite
// runs package binaries concurrently under `-race` while sharing one database
// behind an advisory lock. A budget under a small multiple of the isolated time
// is not a correctness check, it is a machine-speed check, and it fails on
// whichever run happened to be busiest.
const waitForBudget = 2 * time.Minute

// waitFor polls until cond is true or the budget passes.
//
// The pipeline is asynchronous by design — fetch and extract are separate jobs
// precisely so neither blocks the other — so the test waits on the outcome
// rather than on a call returning.
//
// A timeout prints whatever detail is passed. Two minutes of waiting followed by
// "timed out waiting for every article to be localized and written" is a true
// statement that starts an investigation rather than ending one; the same failure
// with the articles' statuses beside it is a diagnosis.
func waitFor(t *testing.T, what string, cond func() bool, detail ...func() string) {
	t.Helper()

	deadline := time.Now().Add(waitForBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	for _, d := range detail {
		t.Log(d())
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The fetch-and-extract pipeline end to end: a pending article is fetched, its raw page is
// stored, and the ladder produces a body.
func TestFetchAndExtractPipeline(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, articlePage)
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/the-article",
		Title:        "The Archived Article",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, _ *river.Client[pgx.Tx]) {
		// The periodic scheduler picks the article up on its startup run, so
		// nothing needs to be enqueued by hand — which also proves the
		// scheduler works.
		waitFor(t, "the article to be fetched and extracted", func() bool {
			_, err := s.CurrentContent(ctx, articleID)
			return err == nil
		})
	})

	ctx := t.Context()

	article, err := s.GetArticle(ctx, articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchOK {
		t.Errorf("FetchStatus = %q, want %q", article.FetchStatus, store.FetchOK)
	}
	if article.RawBlobSHA == "" {
		t.Error("no content hash was recorded for the raw page")
	}
	if article.RawBlobPath == "" {
		t.Fatal("no blob path was recorded for the raw page")
	}

	// The raw page is on disk, gzipped, and recoverable. This is what makes
	// `tome reextract` possible without re-fetching.
	if ok, err := blobs.Exists(ctx, article.RawBlobPath); err != nil || !ok {
		t.Errorf("the raw page is not in the blob store at %s: %v", article.RawBlobPath, err)
	}
	if !strings.HasSuffix(article.RawBlobPath, "raw.html.gz") {
		t.Errorf("raw blob path = %q, want it to end in raw.html.gz", article.RawBlobPath)
	}
	if !strings.HasPrefix(article.RawBlobPath, "articles/") {
		t.Errorf("raw blob path = %q, want it under articles/", article.RawBlobPath)
	}

	content, err := s.CurrentContent(ctx, articleID)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if content.ExtractorVersion != extract.Version {
		t.Errorf("extractor version = %q, want the current %q", content.ExtractorVersion, extract.Version)
	}
	if content.ContentOrigin != store.OriginFetched {
		t.Errorf("content origin = %q, want %q", content.ContentOrigin, store.OriginFetched)
	}
	if !strings.Contains(content.Text, "An archive is only worth having") {
		t.Errorf("the body is missing the article text:\n%s", content.Text)
	}
	for _, chrome := range []string{"Subscribe now for unlimited access", "All rights reserved"} {
		if strings.Contains(content.Text, chrome) {
			t.Errorf("the body contains page chrome: %q", chrome)
		}
	}
	if content.WordCount == 0 {
		t.Error("WordCount = 0")
	}

	// The page usually knows more about itself than the feed did.
	if article.Author == "" && article.SiteName == "" {
		t.Error("no metadata was recovered from the page")
	}
}

// A page that cannot be fetched is recorded as failed, with the reason, rather
// than being retried forever or silently dropped.
func TestFetchFailureIsRecorded(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/gone",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, _ *river.Client[pgx.Tx]) {
		waitFor(t, "the fetch failure to be recorded", func() bool {
			a, err := s.GetArticle(ctx, articleID)
			return err == nil && a.FetchStatus == store.FetchFailed
		})
	})

	article, err := s.GetArticle(t.Context(), articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if article.FetchStatus != store.FetchFailed {
		t.Errorf("FetchStatus = %q, want %q", article.FetchStatus, store.FetchFailed)
	}

	// Nothing was extracted, and that is the correct outcome.
	if _, err := s.CurrentContent(t.Context(), articleID); err == nil {
		t.Error("a body was stored for an article that could not be fetched")
	}
}

// robots.txt refusing a path is not a failure: it will not change on retry,
// and skipped keeps the failed-fetch queue meaningful.
func TestRobotsDisallowedArticleIsSkipped(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /private/\n")
			return
		}
		_, _ = io.WriteString(w, articlePage)
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/private/article",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, _ *river.Client[pgx.Tx]) {
		waitFor(t, "the article to be skipped", func() bool {
			a, err := s.GetArticle(ctx, articleID)
			return err == nil && a.FetchStatus == store.FetchSkipped
		})
	})

	article, err := s.GetArticle(t.Context(), articleID)
	if err != nil {
		t.Fatalf("GetArticle() = %v", err)
	}
	if !strings.Contains(article.FetchError, "robots") {
		t.Errorf("fetch error = %q, want it to name robots.txt", article.FetchError)
	}
}

// A domain rule rescues a page the heuristics get wrong, and reextract applies
// it to an article that is already stored — without re-fetching.
func TestDomainRuleAppliedByReextract(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	const promoHeavy = `<!DOCTYPE html><html lang="en"><head><title>Localizing assets</title></head><body>
	  <div class="promo">Subscribe today and save forty percent on an annual membership. Members get
	  unlimited access to the full archive, the weekly newsletter, exclusive events, and the complete
	  back catalog going back decades. Cancel any time, no questions asked, money back in full.</div>
	  <div data-role="story-body">
	    <p>The measure that matters for an archive is not how many pages were saved but how many still
	    render years later. A saved page that depends on a stylesheet from a lapsed domain is a saved
	    page in name only, and that is why localizing assets is not an optimization.</p>
	  </div>
	</body></html>`

	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		fetches++
		_, _ = io.WriteString(w, promoHeavy)
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/notes",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, riverClient *river.Client[pgx.Tx]) {
		waitFor(t, "the first extraction", func() bool {
			_, err := s.CurrentContent(ctx, articleID)
			return err == nil
		})

		fetchesBefore := fetches

		// Now a rule is written for the domain, and the article is
		// re-extracted from the *stored* page.
		u := mustParseHost(t, srv.URL)
		if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
			Domain:          u,
			ContentSelector: `div[data-role="story-body"]`,
			StripSelectors:  []string{".promo"},
		}); err != nil {
			t.Fatalf("UpsertDomainRule() = %v", err)
		}

		// Exactly what `tome reextract` does: confirm the article is a
		// candidate, then queue a forced extraction.
		candidates, err := s.System().ReextractCandidates(ctx, "never-matches", "", 0, 100)
		if err != nil {
			t.Fatalf("ReextractCandidates() = %v", err)
		}
		if !containsArticle(candidates, articleID) {
			t.Fatal("the article was not offered as a reextract candidate")
		}
		if err := jobs.EnqueueExtraction(ctx, riverClient, articleID, true); err != nil {
			t.Fatalf("EnqueueExtraction() = %v", err)
		}

		waitFor(t, "the re-extraction to use the rule", func() bool {
			c, err := s.CurrentContent(ctx, articleID)
			return err == nil && c.ExtractorName == extract.NameDomainRule
		})

		// The whole point: reprocessing costs no requests to the origin.
		if fetches != fetchesBefore {
			t.Errorf("re-extraction made %d further requests, want 0", fetches-fetchesBefore)
		}
	})

	content, err := s.CurrentContent(t.Context(), articleID)
	if err != nil {
		t.Fatalf("CurrentContent() = %v", err)
	}
	if strings.Contains(content.Text, "Subscribe today") {
		t.Errorf("the rule's strip selector was not applied:\n%s", content.Text)
	}
	if !strings.Contains(content.Text, "The measure that matters") {
		t.Errorf("the rule did not select the article body:\n%s", content.Text)
	}
}

func mustParseHost(t *testing.T, rawURL string) string {
	t.Helper()

	after, ok := strings.CutPrefix(rawURL, "http://")
	if !ok {
		t.Fatalf("unexpected test server URL %q", rawURL)
	}
	host, _, _ := strings.Cut(after, "/")
	if h, _, found := strings.Cut(host, ":"); found {
		return h
	}
	return host
}

func containsArticle(candidates []store.ReextractCandidate, id store.ArticleID) bool {
	for _, c := range candidates {
		if c.ArticleID == id {
			return true
		}
	}
	return false
}

// The acceptance criterion: the same image across ten articles stores once.
//
// Two properties are asserted, and they are different. Storage deduplication
// comes from content-addressing and would hold even if the image were fetched
// ten times. Fetch deduplication comes from the source-URL lookup and is what
// the origin server experiences.
func TestSameImageAcrossArticlesStoresOnce(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	var imageFetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, ".png"):
			atomic.AddInt32(&imageFetches, 1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testImagePNG(t))
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, articleWithImage(srvURLFor(r)))
		}
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	const articles = 10
	ids := make([]store.ArticleID, 0, articles)
	for i := range articles {
		id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
			URLCanonical: fmt.Sprintf("%s/article-%d", srv.URL, i),
			Title:        fmt.Sprintf("Article %d", i),
		})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		ids = append(ids, id)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, _ *river.Client[pgx.Tx]) {
		// Waits on the postconditions this test then asserts, not on a status
		// that is settled earlier.
		//
		// The status alone is not enough: SetAssetsStatus runs before the
		// archive files are written, so a wait on it can return in the gap
		// between them. Observed once as "9 article-to-image references, want
		// 10" together with a missing index.html — a real failure that says
		// nothing about the code, which is the worst kind to leave in CI.
		waitFor(t, "every article to be localized and written", func() bool {
			for _, id := range ids {
				a, err := s.GetArticle(ctx, id)
				if err != nil || a.AssetsStatus == store.AssetsPending {
					return false
				}
			}
			st, err := s.System().Stats(ctx)
			if err != nil || st.AssetLinks != articles {
				return false
			}
			return true
		}, func() string {
			// What the articles actually did, which is the difference between a
			// timeout that names a cause and one that starts an investigation.
			var b strings.Builder
			b.WriteString("article states at the deadline:\n")
			for _, id := range ids {
				a, err := s.GetArticle(ctx, id)
				if err != nil {
					fmt.Fprintf(&b, "  %d: unreadable: %v\n", id, err)
					continue
				}
				fmt.Fprintf(&b, "  %d: fetch=%s assets=%s %s\n",
					id, a.FetchStatus, a.AssetsStatus, a.FetchError)
			}
			if st, err := s.System().Stats(ctx); err == nil {
				fmt.Fprintf(&b, "  asset links: %d, want %d", st.AssetLinks, articles)
			}
			return b.String()
		})
	})

	ctx := t.Context()

	st, err := s.System().Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() = %v", err)
	}

	if st.Assets != 1 {
		t.Errorf("the archive holds %d images for %d articles sharing one, want 1", st.Assets, articles)
	}
	if st.AssetLinks != articles {
		t.Errorf("%d article-to-image references, want %d", st.AssetLinks, articles)
	}
	if got := atomic.LoadInt32(&imageFetches); got != 1 {
		t.Errorf("the origin served the image %d times, want 1", got)
	}

	// And exactly one file on disk.
	var files int
	root := blobs.Root()
	if err := filepath.WalkDir(filepath.Join(root, "assets"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the assets tree: %v", err)
	}
	if files != 1 {
		t.Errorf("the assets tree holds %d files, want 1", files)
	}

	// Every article's page must still resolve its image — sharing a file must
	// not mean nine articles point at nothing.
	for _, id := range ids {
		article, err := s.GetArticle(ctx, id)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}
		dir := blob.ArticleDir(article.FirstSeenAt, article.Title, article.URLCanonical)
		assertImagesResolve(t, filepath.Join(root, dir, archive.IndexFile))
	}
}

// assertImagesResolve opens an archived page from disk, with nothing running,
// and checks every image reference against the filesystem.
func assertImagesResolve(t *testing.T, indexPath string) {
	t.Helper()

	f, err := os.Open(indexPath)
	if err != nil {
		t.Fatalf("opening %s: %v", indexPath, err)
	}
	defer func() { _ = f.Close() }()

	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatalf("parsing %s: %v", indexPath, err)
	}

	images := doc.Find("img[src]")
	if images.Length() == 0 {
		t.Errorf("%s has no images", indexPath)
	}

	images.Each(func(_ int, img *goquery.Selection) {
		src, _ := img.Attr("src")
		if strings.HasPrefix(src, "http") {
			t.Errorf("%s: image %q was not localized", indexPath, src)
			return
		}
		resolved := filepath.Join(filepath.Dir(indexPath), filepath.FromSlash(src))
		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("%s: image %q does not resolve: %v", indexPath, src, err)
		}
	})
}

// testImagePNG is a photograph-like image big enough to clear the size policy.
func testImagePNG(t *testing.T) []byte {
	t.Helper()

	const w, h = 400, 300
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x % 251), uint8(y % 241), uint8((x * y) % 239), 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding the test image: %v", err)
	}
	return buf.Bytes()
}

func articleWithImage(base string) string {
	return `<!DOCTYPE html><html lang="en"><head><title>Shared Image</title></head><body>
	<article>
	<h1>Shared Image</h1>
	<p>Ten articles reference the same illustration, and the archive should hold
	exactly one copy of it. Storing ten would be the single most expensive
	mistake available here, because images are most of what an archive weighs
	and syndicated stories repeat them constantly across sources.</p>
	<p><img src="` + base + `/shared-illustration.png" alt="An illustration"></p>
	<p>The deduplication is by content address, computed over the original bytes
	before any resizing or transcoding, so changing the encoder later does not
	turn one image into two.</p>
	</article></body></html>`
}

// srvURLFor reconstructs the test server's base URL from a request.
func srvURLFor(r *http.Request) string {
	return "http://" + r.Host
}

// An article with nothing to extract from says so, rather than blaming the
// extractors.
//
// The two failures need different responses — one wants a domain rule, the other
// wants to know why the fetch never landed — and they used to be the same sentence
// in the attention queue. That cost a long investigation once: a stray job extracted
// an article that had never been fetched, and "extraction produced no content" sent
// the search toward the extraction ladder.
func TestExtractionWithNothingToExtractFromSaysSo(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// An article with no stored page and no feed body: exactly what a job running
	// against an article that was never fetched sees.
	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: "https://example.com/never-fetched",
		URLOriginal:  "https://example.com/never-fetched",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{UserAgent: "tomekeeper/test", MaxAttempts: 1})

	runPipeline(t, s, blobs, client, func(ctx context.Context, riverClient *river.Client[pgx.Tx]) {
		if err := jobs.EnqueueExtraction(ctx, riverClient, id, true); err != nil {
			t.Fatalf("EnqueueExtraction() = %v", err)
		}

		waitFor(t, "the article to be recorded as failed", func() bool {
			a, err := s.GetArticle(ctx, id)
			return err == nil && a.FetchStatus == store.FetchFailed
		}, func() string {
			a, _ := s.GetArticle(ctx, id)
			return fmt.Sprintf("article %d: fetch=%s error=%q", id, a.FetchStatus, a.FetchError)
		})

		a, err := s.GetArticle(ctx, id)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}
		if !strings.Contains(a.FetchError, "no stored page") {
			t.Errorf("fetch_error = %q, want it to say there was no stored page to extract from",
				a.FetchError)
		}
		// And specifically not the sentence that means the opposite thing.
		if strings.Contains(a.FetchError, "extraction produced no content") {
			t.Errorf("fetch_error blames the extractors for an article that was never fetched: %q",
				a.FetchError)
		}
	})
}

// Fetching a page the archive already has, which the worker refuses unless asked.
//
// The remedy for a problem the stored copy cannot be talked out of: extraction runs
// over stored bytes, so when the bytes are wrong — images behind URLs that have since
// expired, a page that needed a browser before anybody flagged the domain — no amount
// of re-extracting helps and only the origin can.
//
// Both halves are asserted, because the interesting one is the refusal: a re-fetch
// that happened by itself would be this archive spending somebody else's bandwidth
// on a page it already had.
func TestAPageIsFetchedAgainOnlyWhenAsked(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	// Both comfortably past the two-hundred-character floor: a body shorter than that
	// is rejected as a paywall stub, and a fixture that trips the floor fails as
	// "extraction produced no content" a long way from anything this test is about.
	first := `<!DOCTYPE html><html><head><title>Localizing assets</title></head><body><article>
	  <p>The first version of this page. The measure that matters for an archive is not
	  how many pages were saved but how many still render years later, and a saved page
	  depending on a stylesheet from a lapsed domain is a saved page in name only.</p>
	  <p>Which is why localizing assets is not an optimization but the whole of the
	  exercise, repeated for every article the poller brings in.</p>
	</article></body></html>`
	second := `<!DOCTYPE html><html><head><title>Localizing assets</title></head><body><article>
	  <p>The second version, which is what the origin serves now and the whole reason
	  anybody would ask for this page again. Extraction runs over stored bytes, so when
	  the bytes themselves are wrong no amount of re-extracting can help.</p>
	  <p>Only the origin can fix that, and asking it costs a request somebody has to
	  choose to spend.</p>
	</article></body></html>`

	page := first
	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		fetches++
		_, _ = io.WriteString(w, page)
	}))
	defer srv.Close()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	articleID, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{
		URLCanonical: srv.URL + "/notes",
	})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}

	client := httpclient.New(httpclient.Options{
		UserAgent: "tomekeeper/test", MaxAttempts: 1, DefaultRPS: 100,
	})

	runPipeline(t, s, blobs, client, func(ctx context.Context, riverClient *river.Client[pgx.Tx]) {
		// Enqueued rather than waited for: the periodic scheduler would get here
		// eventually, and depending on its timing makes this test slow when it passes
		// and mysterious when it does not.
		if _, err := riverClient.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(articleID)}, nil); err != nil {
			t.Fatalf("Insert() = %v", err)
		}

		waitFor(t, "the first fetch and extraction", func() bool {
			_, err := s.CurrentContent(ctx, articleID)
			return err == nil
		}, func() string {
			a, _ := s.GetArticle(ctx, articleID)
			return fmt.Sprintf("fetches=%d fetch_status=%s error=%q", fetches, a.FetchStatus, a.FetchError)
		})

		before, err := s.GetArticle(ctx, articleID)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}
		if before.Title == "" {
			t.Fatal("extraction did not set a title, so the paths cannot diverge and this proves nothing")
		}
		fetchesAfterFirst := fetches

		// An ordinary enqueue must not re-fetch. This is the guard: the pipeline
		// enqueues fetches freely — three feeds carrying one story, a retry, a
		// scheduler sweep — and any of them re-fetching would be this archive
		// spending somebody else's bandwidth on a page it already had.
		// Inserted directly, which is exactly what the poller and the scheduler do:
		// FetchArticleArgs with nothing set. There is no helper for it, and that is
		// the point — Again has one caller.
		res, err := riverClient.Insert(ctx, jobs.FetchArticleArgs{ArticleID: int64(articleID)}, nil)
		if err != nil {
			t.Fatalf("Insert() = %v", err)
		}
		// The insert has to actually happen, or the assertion below passes because
		// River deduplicated it rather than because the worker declined to act.
		if res.UniqueSkippedAsDuplicate {
			t.Fatalf("the second fetch was deduplicated, so this proves nothing about the worker")
		}
		// Nothing is written when the job declines to act, so there is no state to
		// wait for — only the absence of one. A settle is the honest instrument here.
		time.Sleep(750 * time.Millisecond)
		if fetches != fetchesAfterFirst {
			t.Errorf("an ordinary enqueue re-fetched the page: %d requests then %d",
				fetchesAfterFirst, fetches)
		}

		// Asked explicitly, it fetches and the stored page is the new one.
		page = second
		if err := jobs.EnqueueRefetch(ctx, riverClient, articleID); err != nil {
			t.Fatalf("EnqueueRefetch() = %v", err)
		}

		waitFor(t, "the body to come from the second version", func() bool {
			c, err := s.CurrentContent(ctx, articleID)
			return err == nil && strings.Contains(c.Text, "second version")
		}, func() string {
			c, _ := s.CurrentContent(ctx, articleID)
			return fmt.Sprintf("body is %q, fetches=%d", c.Text, fetches)
		})

		if fetches <= fetchesAfterFirst {
			t.Errorf("a re-fetch made no request: %d then %d", fetchesAfterFirst, fetches)
		}

		after, err := s.GetArticle(ctx, articleID)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}

		// What makes the assertion below reachable, and it took a neuter to get right:
		// this article was created with no title, so its first fetch named the
		// directory from the URL slug, and the extraction that followed filled the
		// title in. Recomputing the path now would name it after that title instead.
		//
		// The page's title therefore has to slugify *differently* from its URL path —
		// "Localizing assets" against /notes. With a title that happened to match the
		// slug the two paths coincided, the assertion held for the wrong reason, and
		// removing the code under test changed nothing.

		// **Overwritten in place, not stored beside.** The article's directory holds
		// its page, its index.html and its localized images, and a re-fetch that
		// picked a new directory would separate them and orphan the old page. The
		// directory's name comes from the title, which extraction updates — so this
		// is reachable rather than theoretical, and the fix is to reuse the path the
		// article already has.
		if after.RawBlobPath != before.RawBlobPath {
			t.Errorf("the page moved to a new path: %q then %q — an article's files must stay together",
				before.RawBlobPath, after.RawBlobPath)
		}
		if after.RawBlobSHA == before.RawBlobSHA {
			t.Errorf("the stored page's checksum did not change, so the new bytes were not recorded")
		}
		if ok, err := blobs.Exists(ctx, after.RawBlobPath); err != nil {
			t.Fatalf("Exists() = %v", err)
		} else if !ok {
			t.Errorf("no page is stored at %q", after.RawBlobPath)
		}
	})
}
