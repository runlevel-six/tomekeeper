package jobs

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// What an extraction that produces nothing should record depends on whether the
// article already has a body, and this calls Work directly to find out.
//
// Synchronous on purpose. Three attempts at this through the job queue were all
// races that passed with the fix removed: the extraction attempt is recorded
// before the branch under test, a count of completed jobs is satisfied by the
// *previous* extraction being marked done, and waiting on the inserted job's own
// state never terminated. Asserting that something is *not* written is exactly
// the case where "the work has probably finished by now" is not good enough.
func TestWhatAnEmptyExtractionRecords(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	blobs, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}

	// A page with nothing any rung will accept.
	const shell = `<!DOCTYPE html><html lang="en"><head><title>Notes</title></head>` +
		`<body><div id="app"></div></body></html>`
	// Gzipped, because that is how the fetcher stores a page and readRaw always
	// decompresses. Storing it plain made Work fail on the read instead of
	// reaching the branch under test, which is what made two earlier attempts at
	// this test look like a scheduling race.
	const shellPath = "articles/test/shell/raw.html.gz"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(shell)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := blobs.Put(ctx, shellPath, bytes.NewReader(gz.Bytes())); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	worker := &ExtractArticleWorker{
		store:     s,
		blobs:     blobs,
		extractor: extract.New(),
		log:       slog.New(slog.DiscardHandler),
	}

	// A page stored against an article, so the fetch half is demonstrably fine and
	// only the extraction produces nothing.
	newArticle := func(t *testing.T, url string) store.ArticleID {
		t.Helper()
		id, _, err := s.UpsertArticle(ctx, store.ArticleParams{URLCanonical: url, URLOriginal: url})
		if err != nil {
			t.Fatalf("UpsertArticle() = %v", err)
		}
		if err := s.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: "sha-" + url, Path: shellPath}); err != nil {
			t.Fatalf("RecordFetchSuccess() = %v", err)
		}
		return id
	}

	run := func(t *testing.T, id store.ArticleID) {
		t.Helper()
		err := worker.Work(ctx, &river.Job[ExtractArticleArgs]{
			JobRow: &rivertype.JobRow{},
			Args:   ExtractArticleArgs{ArticleID: int64(id), Force: true},
		})
		if err != nil {
			t.Fatalf("Work() = %v", err)
		}
	}

	t.Run("with a body already, nothing is recorded", func(t *testing.T) {
		id := newArticle(t, "https://example.com/has-a-body")

		// A body from older behavior, of the kind a version bump can strand: the
		// ladder cannot produce it from this page any more, and the reader has it.
		if _, err := s.InsertContent(ctx, store.ContentParams{
			ArticleID:        id,
			ExtractorName:    extract.NameTrafilatura,
			ExtractorVersion: "1",
			ContentOrigin:    store.OriginFetched,
			HTML:             "<p>A body produced by an earlier version of the ladder.</p>",
			Text:             "A body produced by an earlier version of the ladder.",
			WordCount:        10,
		}); err != nil {
			t.Fatalf("InsertContent() = %v", err)
		}

		run(t, id)

		a, err := s.GetArticle(ctx, id)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}
		if a.FetchStatus != store.FetchOK {
			t.Errorf("fetch_status = %q for an article that still has its body, want %q — this is what filled the attention queue with work nobody could do",
				a.FetchStatus, store.FetchOK)
		}
		if a.FetchError != "" {
			t.Errorf("fetch_error = %q, want nothing recorded", a.FetchError)
		}

		body, err := s.CurrentContent(ctx, id)
		if err != nil {
			t.Fatalf("CurrentContent() = %v", err)
		}
		if body.ExtractorVersion != "1" {
			t.Errorf("the older body was replaced: version = %q, want %q kept",
				body.ExtractorVersion, "1")
		}

		// And the attempt is still recorded, so a later reprocess can tell this
		// page has been seen by this version.
		if a.ExtractAttemptVersion != extract.Version {
			t.Errorf("extract_attempt_version = %q, want %q", a.ExtractAttemptVersion, extract.Version)
		}
	})

	// The counterweight, and the reason the guard is narrow: an article with no
	// body and nothing extractable is exactly what the attention queue is for.
	t.Run("with no body, the failure is recorded", func(t *testing.T) {
		id := newArticle(t, "https://example.com/has-no-body")

		run(t, id)

		a, err := s.GetArticle(ctx, id)
		if err != nil {
			t.Fatalf("GetArticle() = %v", err)
		}
		if a.FetchStatus != store.FetchFailed {
			t.Errorf("fetch_status = %q for a bodyless article nothing could extract, want %q",
				a.FetchStatus, store.FetchFailed)
		}
		if !strings.Contains(a.FetchError, "extraction produced no content") {
			t.Errorf("fetch_error = %q, want it to name the extractors", a.FetchError)
		}
	})

}
