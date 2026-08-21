package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// EnqueueExtraction queues one article for extraction.
//
// force re-runs extraction even when a current body already exists at the
// current extractor version. `tome reextract` sets it, because the articles it
// selects all have a body already; the fetch pipeline does not, so a
// duplicated job is cheap rather than wasteful.
func EnqueueExtraction(ctx context.Context, client *river.Client[pgx.Tx], id store.ArticleID, force bool) error {
	if _, err := client.Insert(ctx, ExtractArticleArgs{
		ArticleID: int64(id),
		Force:     force,
	}, nil); err != nil {
		return fmt.Errorf("queueing extraction of article %d: %w", id, err)
	}
	return nil
}

// EnqueueRefetch asks for a page this archive already has to be fetched again,
// replacing what is stored.
//
// Separate from the pipeline's own enqueue and never called by it: the fetch worker
// refuses a page it already has unless asked, because a re-fetch is a request the
// origin did not need to serve. This is the only thing that asks.
func EnqueueRefetch(ctx context.Context, client *river.Client[pgx.Tx], id store.ArticleID) error {
	if _, err := client.Insert(ctx, FetchArticleArgs{
		ArticleID: int64(id),
		Again:     true,
	}, nil); err != nil {
		return fmt.Errorf("enqueueing a re-fetch of article %d: %w", id, err)
	}
	return nil
}
