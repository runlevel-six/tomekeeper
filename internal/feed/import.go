package feed

import (
	"context"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// ImportResult reports what an import did.
//
// Three outcomes rather than two, because "already subscribed" is not a failure
// and not a change: re-running an import is the natural way to recover from one
// that stopped halfway, and a reader doing that should be told plainly that
// nothing new happened rather than being shown a wall of green.
type ImportResult struct {
	Added    int
	Existing int
	Failures []ImportFailure
}

// ImportFailure is one subscription that could not be stored, and why.
type ImportFailure struct {
	FeedURL string
	Title   string
	Err     error
}

// Total is how many subscriptions were attempted.
func (r ImportResult) Total() int {
	return r.Added + r.Existing + len(r.Failures)
}

// Import subscribes a user to every subscription in subs.
//
// One bad subscription must not cost the other four hundred. A feed URL that the
// database rejects — malformed, absurdly long, whatever a decade-old reader
// exported — is collected into Failures and the import carries on. An OPML file
// exported from a long-lived account is exactly where a single bad row is most
// likely and least acceptable as a reason to lose the rest.
//
// Subscriptions are keyed by (user, feed URL), so this is idempotent: importing
// the same file twice updates titles and categories and creates nothing.
//
// Shared by `tome import-opml` and the web upload, so the two cannot disagree
// about what an import means.
func Import(ctx context.Context, s *store.Store, userID store.UserID, subs []Subscription) ImportResult {
	var result ImportResult

	for _, sub := range subs {
		_, isNew, err := s.UpsertFeed(ctx, userID, store.FeedParams{
			FeedURL:  sub.FeedURL,
			SiteURL:  sub.SiteURL,
			Title:    sub.Title,
			Category: sub.Category,
		})
		switch {
		case err != nil:
			result.Failures = append(result.Failures, ImportFailure{
				FeedURL: sub.FeedURL,
				Title:   sub.Title,
				Err:     err,
			})
		case isNew:
			result.Added++
		default:
			result.Existing++
		}
	}

	return result
}
