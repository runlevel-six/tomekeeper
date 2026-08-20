// Package feed polls subscriptions and turns their entries into article
// references.
//
// Nothing here fetches article pages or extracts content — those are separate
// jobs. A poll
// leaves new articles at fetch_status='pending', which is the queue the
// fetcher will drain.
package feed

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/urlcanon"
)

// Store is the subset of the data layer a poll needs.
//
// It is an interface so that the polling logic — conditional GET, dedupe
// within a poll, interval selection, failure escalation — can be tested
// against an in-memory double rather than only against a live Postgres. That
// matters because these are the paths that are hard to provoke on demand with
// real feeds: a 304, a malformed body, the twentieth consecutive failure.
//
// Every user-scoped method keeps its UserID here too. The indirection must not
// become the place where scoping quietly goes missing.
type Store interface {
	GetFeed(ctx context.Context, userID store.UserID, feedID store.FeedID) (store.Feed, error)
	UpsertArticle(ctx context.Context, p store.ArticleParams) (store.ArticleID, bool, error)
	InsertFeedItem(ctx context.Context, userID store.UserID, p store.FeedItemParams) (bool, error)
	RecordPollSuccess(ctx context.Context, userID store.UserID, feedID store.FeedID,
		etag, lastModified string, interval time.Duration) error
	RecordPollNotModified(ctx context.Context, userID store.UserID, feedID store.FeedID,
		interval time.Duration) error
	RecordPollFailure(ctx context.Context, userID store.UserID, feedID store.FeedID,
		cause string, interval time.Duration, disableAfter int) (bool, error)
}

// Poller fetches one feed and records what it found.
type Poller struct {
	store  Store
	client *httpclient.Client
	policy IntervalPolicy
	log    *slog.Logger

	// disableAfter is the number of consecutive failures after which a feed is
	// disabled and surfaced in the UI. A feed is never silently dropped.
	disableAfter int
}

// NewPoller returns a Poller.
func NewPoller(s Store, c *httpclient.Client, policy IntervalPolicy, disableAfter int, log *slog.Logger) *Poller {
	return &Poller{
		store:        s,
		client:       c,
		policy:       policy,
		log:          log,
		disableAfter: disableAfter,
	}
}

// Result summarizes one poll.
type Result struct {
	NotModified bool
	Skipped     bool // the feed is disabled
	TotalItems  int
	NewItems    int // references new to this feed
	NewArticles int // articles new to the archive

	// NewArticleIDs are the articles this poll added to the archive.
	//
	// The poller returns them rather than enqueueing the fetches itself, so
	// that this package knows nothing about the job queue. The worker turns
	// them into fetch_article jobs.
	NewArticleIDs []store.ArticleID
	NextInterval  time.Duration
	Disabled      bool // the feed was disabled by this failure
}

// Poll fetches a feed and records its entries.
//
// Failures are recorded on the feed rather than returned as job errors
// wherever the failure is the feed's rather than ours: a site being down is
// expected operational reality, not a bug to retry aggressively. The error
// return is reserved for problems on this side — a database that will not
// accept writes, say.
func (p *Poller) Poll(ctx context.Context, userID store.UserID, feedID store.FeedID) (Result, error) {
	f, err := p.store.GetFeed(ctx, userID, feedID)
	if err != nil {
		return Result{}, err
	}
	if f.Disabled {
		return Result{Skipped: true}, nil
	}

	log := p.log.With("feed_id", int64(feedID), "feed_url", f.FeedURL)

	resp, err := p.client.Do(ctx, httpclient.Request{
		URL:    f.FeedURL,
		Header: conditionalHeaders(f),
		// A feed is a subscription the reader asked for, published in a format
		// whose whole purpose is automated consumption. Article and asset
		// fetches are subject to robots.txt; this is not. See the politeness rules.
		SkipRobots: true,
	})
	if err != nil {
		return p.recordFailure(ctx, userID, f, err.Error(), log)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// The cheap path, and the one the acceptance criteria are about: a
		// second poll of an unchanged feed should mostly land here, having
		// transferred no body at all.
		// Nil rather than a parsed feed, because a 304 has no body to read a
		// declared cadence out of. Everything else about the decision is the same,
		// including a cadence the reader chose.
		interval := p.nextInterval(f, nil, false)
		if err := p.store.RecordPollNotModified(ctx, userID, feedID, interval); err != nil {
			return Result{}, err
		}
		log.Debug("feed not modified", "next_interval", interval)
		return Result{NotModified: true, NextInterval: interval}, nil

	case resp.StatusCode < 200 || resp.StatusCode > 299:
		cause := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			cause += " (Retry-After: " + retryAfter + ")"
		}
		return p.recordFailure(ctx, userID, f, cause, log)
	}

	body, err := httpclient.ReadBody(resp.Body)
	if err != nil {
		return p.recordFailure(ctx, userID, f, err.Error(), log)
	}

	// A parser per poll rather than one shared: gofeed's Parser holds
	// per-parse state and is not documented as safe for concurrent use, and
	// workers poll feeds in parallel.
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return p.recordFailure(ctx, userID, f, "parsing feed: "+err.Error(), log)
	}

	result, err := p.ingest(ctx, userID, f, parsed, log)
	if err != nil {
		return Result{}, err
	}

	result.NextInterval = p.nextInterval(f, parsed, result.NewItems > 0)

	if err := p.store.RecordPollSuccess(ctx, userID, feedID,
		resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), result.NextInterval,
	); err != nil {
		return Result{}, err
	}

	log.Info("polled feed",
		"items", result.TotalItems,
		"new_items", result.NewItems,
		"new_articles", result.NewArticles,
		"next_interval", result.NextInterval,
	)
	return result, nil
}

// ingest walks a parsed feed's entries, upserting an article per entry and a
// feed item per reference.
func (p *Poller) ingest(ctx context.Context, userID store.UserID, f store.Feed,
	parsed *gofeed.Feed, log *slog.Logger,
) (Result, error) {
	result := Result{TotalItems: len(parsed.Items)}

	// Within a single poll, a feed that lists the same entry twice — which
	// happens, particularly around pagination bugs — must not produce two
	// database round trips or two references.
	seen := make(map[string]bool, len(parsed.Items))

	base := feedBaseURL(f)

	for _, item := range parsed.Items {
		link := resolveLink(base, item.Link)
		if link == "" {
			log.Warn("feed entry has no link, skipping", "title", item.Title)
			continue
		}

		canonical, err := urlcanon.Canonicalize(link)
		if err != nil {
			// One unusable link must not abort a poll that has 40 good ones.
			log.Warn("feed entry has an uncanonicalizable link, skipping",
				"link", link, "error", err)
			continue
		}

		// The poller design: deduplicate within a poll by GUID, falling back to the
		// canonical URL when the feed does not supply one.
		guid := strings.TrimSpace(item.GUID)
		if guid == "" {
			guid = canonical
		}
		if seen[guid] {
			continue
		}
		seen[guid] = true

		articleID, created, err := p.store.UpsertArticle(ctx, store.ArticleParams{
			URLCanonical: canonical,
			URLOriginal:  link,
			Title:        strings.TrimSpace(item.Title),
			Author:       itemAuthor(item),
			SiteName:     parsed.Title,
			Language:     parsed.Language,
			PublishedAt:  itemPublished(item),
		})
		if err != nil {
			return Result{}, err
		}
		if created {
			result.NewArticles++
			result.NewArticleIDs = append(result.NewArticleIDs, articleID)
		}

		inserted, err := p.store.InsertFeedItem(ctx, userID, store.FeedItemParams{
			FeedID:    f.ID,
			ArticleID: articleID,
			GUID:      guid,
			Title:     strings.TrimSpace(item.Title),
			Summary:   strings.TrimSpace(item.Description),
			Content:   strings.TrimSpace(item.Content),
		})
		if err != nil {
			return Result{}, err
		}
		if inserted {
			result.NewItems++
		}
	}
	return result, nil
}

// nextInterval decides when to poll this feed again.
//
// In order: a cadence the reader chose for this feed, then their general
// preference, then one the feed declares for itself, then what the poll found.
//
// A reader's choice beats the feed's own declaration, which is worth saying because
// the opposite is arguable — the publisher does know their own schedule. But an
// explicit setting whose effect depends on markup the reader cannot see is not a
// setting, it is a suggestion: somebody who asks for hourly checks on a feed
// carrying `sy:updatePeriod` of daily would get daily, be told nothing, and have no
// way to find out why. The declaration is still better information than the
// estimate, so it keeps its place ahead of the adaptive interval.
//
// parsed may be nil, for a 304 that carried no body.
func (p *Poller) nextInterval(f store.Feed, parsed *gofeed.Feed, foundNew bool) time.Duration {
	if chosen, ok := f.ChosenInterval(); ok {
		return p.policy.Chosen(chosen)
	}
	if hint, ok := syUpdateHint(p.policy, parsed); ok {
		return hint
	}
	if foundNew {
		return p.policy.OnNewItems(f.PollInterval)
	}
	return p.policy.OnNoChange(f.PollInterval)
}

// recordFailure notes a failed poll and backs off.
//
// A cadence the reader chose does not shorten the backoff — a feed that is down is
// not a feed whose publishing schedule is the question — but it does act as a floor
// on it. Without that, the first failure on a feed set to weekly would drop it to
// the 15-minute floor the backoff starts from, so a broken feed would be polled
// hundreds of times more often than the reader asked for a working one.
func (p *Poller) recordFailure(ctx context.Context, userID store.UserID, f store.Feed,
	cause string, log *slog.Logger,
) (Result, error) {
	interval := p.policy.OnFailure(f.ConsecutiveFailures + 1)
	if chosen, ok := f.ChosenInterval(); ok {
		interval = max(interval, p.policy.Chosen(chosen))
	}

	disabled, err := p.store.RecordPollFailure(ctx, userID, f.ID, cause, interval, p.disableAfter)
	if err != nil {
		return Result{}, err
	}

	if disabled {
		log.Error("feed disabled after repeated failures",
			"failures", f.ConsecutiveFailures+1, "error", cause)
	} else {
		log.Warn("feed poll failed",
			"failures", f.ConsecutiveFailures+1, "error", cause, "next_interval", interval)
	}
	return Result{NextInterval: interval, Disabled: disabled}, nil
}

// conditionalHeaders builds the validators that let an unchanged feed answer
// 304 with no body.
func conditionalHeaders(f store.Feed) http.Header {
	h := make(http.Header, 2)
	if f.ETag != "" {
		h.Set("If-None-Match", f.ETag)
	}
	if f.LastModified != "" {
		h.Set("If-Modified-Since", f.LastModified)
	}
	return h
}

// feedBaseURL returns the URL relative entry links should be resolved against.
func feedBaseURL(f store.Feed) *url.URL {
	for _, candidate := range []string{f.SiteURL, f.FeedURL} {
		if candidate == "" {
			continue
		}
		if u, err := url.Parse(candidate); err == nil && u.Host != "" {
			return u
		}
	}
	return nil
}

// resolveLink turns a possibly-relative entry link into an absolute URL.
//
// Feeds carry relative links more often than the specifications would suggest,
// and a relative link is not canonicalizable, so without this those entries
// would be dropped.
func resolveLink(base *url.URL, link string) string {
	link = strings.TrimSpace(link)
	if link == "" || base == nil {
		return link
	}

	ref, err := url.Parse(link)
	if err != nil {
		return link
	}
	if ref.IsAbs() {
		return link
	}
	return base.ResolveReference(ref).String()
}

// itemPublished picks the best available timestamp for an entry.
func itemPublished(item *gofeed.Item) *time.Time {
	if item.PublishedParsed != nil {
		return item.PublishedParsed
	}
	// An entry that has only ever been updated is dated by that update; it is
	// a worse answer than a publication date but a much better one than null.
	return item.UpdatedParsed
}

// itemAuthor returns the entry's first named author, if any.
func itemAuthor(item *gofeed.Item) string {
	for _, a := range item.Authors {
		if a != nil && strings.TrimSpace(a.Name) != "" {
			return strings.TrimSpace(a.Name)
		}
	}
	if item.Author != nil {
		return strings.TrimSpace(item.Author.Name)
	}
	return ""
}

// syUpdateHint reads the syndication module's declared cadence, if present.
//
// A nil feed is the 304 case — nothing was transferred, so there is nothing to
// read — and answers the same way a feed without the extension does.
func syUpdateHint(policy IntervalPolicy, parsed *gofeed.Feed) (time.Duration, bool) {
	if parsed == nil {
		return 0, false
	}
	sy, ok := parsed.Extensions["sy"]
	if !ok {
		return 0, false
	}

	first := func(name string) string {
		values, ok := sy[name]
		if !ok || len(values) == 0 {
			return ""
		}
		return values[0].Value
	}

	return policy.FromHint(first("updatePeriod"), first("updateFrequency"))
}
