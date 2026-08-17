package feed_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// fakeStore is an in-memory stand-in for the data layer, recording what a poll
// did so the test can assert on it. It implements feed.Store.
//
// It deliberately reproduces the two behaviors the real schema enforces and
// the poller relies on: articles are keyed by canonical URL across all feeds
// (so duplicates collapse), and feed items are keyed by (feed, guid) (so a
// re-listed entry is not a new reference).
type fakeStore struct {
	mu sync.Mutex

	feed    store.Feed
	feedErr error

	articles map[string]store.ArticleID // canonical URL -> id
	items    map[string]bool            // "feedID\x00guid" -> present
	nextID   store.ArticleID

	// Recorded outcomes.
	successes    []successCall
	notModified  []time.Duration
	failures     []failureCall
	upsertErr    error
	insertItemAt int // return an error on the Nth InsertFeedItem call, 0 = never
	insertCalls  int
}

type successCall struct {
	ETag, LastModified string
	Interval           time.Duration
}

type failureCall struct {
	Cause        string
	Interval     time.Duration
	DisableAfter int
}

func newFakeStore(f store.Feed) *fakeStore {
	return &fakeStore{
		feed:     f,
		articles: make(map[string]store.ArticleID),
		items:    make(map[string]bool),
	}
}

func (s *fakeStore) GetFeed(_ context.Context, _ store.UserID, _ store.FeedID) (store.Feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.feedErr != nil {
		return store.Feed{}, s.feedErr
	}
	return s.feed, nil
}

func (s *fakeStore) UpsertArticle(_ context.Context, p store.ArticleParams) (store.ArticleID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.upsertErr != nil {
		return 0, false, s.upsertErr
	}
	if id, ok := s.articles[p.URLCanonical]; ok {
		return id, false, nil
	}
	s.nextID++
	s.articles[p.URLCanonical] = s.nextID
	return s.nextID, true, nil
}

func (s *fakeStore) InsertFeedItem(_ context.Context, _ store.UserID, p store.FeedItemParams) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.insertCalls++
	if s.insertItemAt != 0 && s.insertCalls == s.insertItemAt {
		return false, fmt.Errorf("simulated database failure")
	}

	key := fmt.Sprintf("%d\x00%s", p.FeedID, p.GUID)
	if s.items[key] {
		return false, nil
	}
	s.items[key] = true
	return true, nil
}

func (s *fakeStore) RecordPollSuccess(_ context.Context, _ store.UserID, _ store.FeedID,
	etag, lastModified string, interval time.Duration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successes = append(s.successes, successCall{etag, lastModified, interval})
	return nil
}

func (s *fakeStore) RecordPollNotModified(_ context.Context, _ store.UserID, _ store.FeedID,
	interval time.Duration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notModified = append(s.notModified, interval)
	return nil
}

func (s *fakeStore) RecordPollFailure(_ context.Context, _ store.UserID, _ store.FeedID,
	cause string, interval time.Duration, disableAfter int,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failureCall{cause, interval, disableAfter})
	return s.feed.ConsecutiveFailures+1 >= disableAfter, nil
}

func (s *fakeStore) articleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.articles)
}

func (s *fakeStore) hasArticle(canonical string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.articles[canonical]
	return ok
}
