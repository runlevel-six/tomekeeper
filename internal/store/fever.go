package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The queries behind the Fever API.
//
// Fever's data model and this one disagree in one structural way, and every
// decision here follows from how that is resolved. In Fever an *item* belongs to
// exactly one feed. Here the article is the root entity and a feed reference is one
// of several ways to reach it, so a story carried by three subscriptions is one
// article — see docs/explanation/why-articles-are-the-root-entity.md.
//
// So a Fever item id is an article id. The alternative, a feed_items id, would give
// Fever the per-feed item it expects and cost two things that matter more: a
// syndicated story would appear once per subscription in every client, and an
// article the reader saved by hand has no feed_items row at all, which would make
// saved_item_ids able to name items that items could never return.

// UserByAPIKey resolves a Fever api_key to the user it authenticates.
//
// On SystemStore for the same reason Credentials is: resolving the credential *is*
// the operation, so there is no user to scope to yet. The scoping discipline puts
// every cross-user lookup here precisely so that the exceptions are greppable.
//
// An empty key is refused before it reaches the database. users.api_key is NULL
// until a password is set, and while NULL never equals anything in SQL, a caller
// that can ask "who has no key" of a table is a caller worth not having.
func (s *SystemStore) UserByAPIKey(ctx context.Context, apiKey string) (UserID, error) {
	if apiKey == "" {
		return 0, fmt.Errorf("fever api key must not be empty")
	}

	var id UserID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE api_key = $1`, apiKey,
	).Scan(&id)
	if err != nil {
		// Deliberately not naming the key in the error. It is a credential, and this
		// error is logged on every failed authentication attempt.
		return 0, fmt.Errorf("looking up a user by api key: %w", err)
	}
	return id, nil
}

// FeverItemQuery is one page of items in Fever's own terms.
//
// The four fields are the three paging arguments the protocol defines plus the
// unbounded-newest case, and they are separate rather than one cursor because the
// protocol makes them separate: a client walks forward with since_id, backward with
// max_id, and fetches a specific set with with_ids. Which one is set is decided by
// which query parameter the client sent, so the handler's dispatch and this struct
// are the same shape on purpose.
type FeverItemQuery struct {
	// SinceID selects items with a greater id, oldest first. This is how a client
	// with a warm cache asks for what has arrived since.
	SinceID ArticleID

	// MaxID selects items with a smaller id, newest first, which is how a client
	// walks backwards into the archive.
	MaxID ArticleID

	// Newest orders newest-first with no upper bound.
	//
	// This is what `max_id=0` means. The spec words it as "the lowest id of locally
	// cached items (or 0 initially)", so taken literally the initial request asks for
	// items with an id below zero and every client's first sync would return nothing.
	// It is a real compatibility detail rather than a reading of the prose: a client
	// starting from empty sends 0 and expects the newest page.
	Newest bool

	// IDs selects exactly these items, and takes precedence over everything above.
	IDs []ArticleID

	// Limit caps the page. Zero means FeverItemLimit, and the protocol's own
	// documented page size is 50.
	Limit int
}

// FeverItemLimit is the page size the Fever protocol specifies for items.
//
// Fifty, and not configurable: clients page by repeating a request until the array
// comes back empty, so a server that returned a different number would still be
// correct — but the number is documented on the client side of a protocol whose
// implementations are no longer being written, and there is nothing to gain by
// finding out which of them hardcoded it.
const FeverItemLimit = 50

// FeverItem is one item as the Fever protocol describes it.
type FeverItem struct {
	ArticleID ArticleID

	// FeedID is the feed this item is attributed to, or zero when none of the
	// reader's feeds carries the article — which is the ordinary case for a page they
	// saved by hand.
	//
	// Chosen the same way the web interface chooses it: the reader's feed that saw
	// the article first. A syndicated story reaches them through one of several
	// subscriptions and that is the honest attribution, but the important part is
	// that the rule is the same in both places, because a client grouping items by
	// feed would otherwise disagree with the archive about where an article came
	// from.
	FeedID FeedID

	Title  string
	Author string

	// HTML is the extracted body, with image sources left exactly as stored. The
	// caller rewrites them, because whether a URL is reachable depends on who is
	// asking and the store has no opinion about that.
	HTML string

	URL     string
	Read    bool
	Starred bool

	// CreatedAt is the article's place in reading order — its publication date where
	// the feed gave one, its arrival otherwise. The same expression the streams sort
	// on, so a client's ordering agrees with the web interface's.
	CreatedAt time.Time
}

// FeverItems returns one page of items.
//
// Ordered by article id rather than by date, which is the whole basis of the
// protocol's paging: since_id and max_id are id comparisons, and ids here ascend
// with arrival because articles.id is a bigserial. The consequence worth knowing is
// in the reference documentation — an article already in the archive that a newly
// added subscription starts carrying keeps its old id, so a client polling with
// since_id will not see it, and will pick it up from unread_item_ids instead.
func (s *Store) FeverItems(ctx context.Context, userID UserID, q FeverItemQuery) ([]FeverItem, error) {
	limit := q.Limit
	if limit <= 0 || limit > FeverItemLimit {
		limit = FeverItemLimit
	}

	where := []string{visibleArticles}
	args := []any{userID}

	// Ascending unless a client is walking backwards, which is what both of the
	// newest-first cases are doing.
	order := "ASC"

	switch {
	case len(q.IDs) > 0:
		raw := make([]int64, len(q.IDs))
		for i, id := range q.IDs {
			raw[i] = int64(id)
		}
		args = append(args, raw)
		where = append(where, fmt.Sprintf("a.id = ANY($%d)", len(args)))
	case q.SinceID > 0:
		args = append(args, q.SinceID)
		where = append(where, fmt.Sprintf("a.id > $%d", len(args)))
	case q.MaxID > 0:
		args = append(args, q.MaxID)
		where = append(where, fmt.Sprintf("a.id < $%d", len(args)))
		order = "DESC"
	case q.Newest:
		order = "DESC"
	}

	args = append(args, limit)

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, COALESCE(feed.id, 0),
		       COALESCE(a.title, ''), COALESCE(a.author, ''),
		       COALESCE(c.content_html, ''), a.url_canonical,
		       COALESCE(st.read, false), COALESCE(st.starred, false),
		       COALESCE(a.published_at, a.first_seen_at)
		FROM articles a
		`+ownedBody+`
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		LEFT JOIN LATERAL (
			SELECT f3.id
			FROM feed_items fi3 JOIN feeds f3 ON f3.id = fi3.feed_id
			WHERE fi3.article_id = a.id AND f3.user_id = $1
			ORDER BY fi3.seen_at, fi3.id
			LIMIT 1
		) feed ON true
		WHERE `+strings.Join(where, "\n		  AND ")+`
		ORDER BY a.id `+order+`
		LIMIT $`+fmt.Sprint(len(args)),
		args...)
	if err != nil {
		return nil, fmt.Errorf("listing fever items for user %d: %w", userID, err)
	}
	defer rows.Close()

	items := make([]FeverItem, 0, limit)
	for rows.Next() {
		var it FeverItem
		if err := rows.Scan(&it.ArticleID, &it.FeedID, &it.Title, &it.Author,
			&it.HTML, &it.URL, &it.Read, &it.Starred, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning a fever item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// CountVisibleArticles counts everything the reader can see.
//
// This is Fever's total_items, and it counts what FeverItems can return rather than
// what their subscriptions carry — so it includes pages they saved by hand, exactly
// as the item pages do. CountUserArticles answers the narrower question and is not
// interchangeable with this one.
func (s *Store) CountVisibleArticles(ctx context.Context, userID UserID) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM articles a
		WHERE `+visibleArticles, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting visible articles for user %d: %w", userID, err)
	}
	return n, nil
}

// UnreadArticleIDs lists every unread article the reader can see.
//
// Deliberately unbounded, which is unusual here and is the protocol's requirement
// rather than an oversight: a client reconciles its cache against this list, so a
// truncated one is not a smaller answer but a wrong one — every id past the cut
// would read as "no longer unread". The archive it is measured against holds a few
// thousand articles and the response is a few tens of kilobytes.
func (s *Store) UnreadArticleIDs(ctx context.Context, userID UserID) ([]ArticleID, error) {
	return s.articleIDs(ctx, userID, `NOT COALESCE(st.read, false)`,
		"listing unread article ids")
}

// StarredArticleIDs lists every starred article the reader can see.
//
// Starred, not saved, and the distinction is load-bearing. Fever has one flag where
// this archive has two: starring is a reaction to something a feed brought, and
// saved_at additionally records the moment a page was archived — which starring also
// sets, and unstarring deliberately does not clear, so that an article stays
// reachable after the feed that introduced it is gone. That makes saved_at
// one-directional and therefore unusable as the mapping: a client unsaving an item
// could never see it take effect. Starred round-trips, so is_saved is starred.
func (s *Store) StarredArticleIDs(ctx context.Context, userID UserID) ([]ArticleID, error) {
	return s.articleIDs(ctx, userID, `COALESCE(st.starred, false)`,
		"listing starred article ids")
}

// articleIDs is the shared body of the two id lists above.
//
// One definition because they differ by a single predicate, and the part that must
// not differ — the visibility bound and the ordering — is the part a second copy
// would eventually get wrong.
func (s *Store) articleIDs(ctx context.Context, userID UserID, predicate, what string) ([]ArticleID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id FROM articles a
		LEFT JOIN article_state st ON st.article_id = a.id AND st.user_id = $1
		WHERE `+visibleArticles+`
		  AND `+predicate+`
		ORDER BY a.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("%s for user %d: %w", what, userID, err)
	}
	defer rows.Close()

	var ids []ArticleID
	for rows.Next() {
		var id ArticleID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning an article id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LastRefreshedAt is when any of the reader's feeds was last polled.
//
// Fever's last_refreshed_on_time, and the spec is precise in a way worth honoring:
// it is the most recently *refreshed* feed, "not updated". So this reads
// last_polled_at — when the archive last went and looked — rather than
// last_success_at, which is when a feed last actually had something to give. The
// per-feed last_updated_on_time is the other one, for the same reason.
//
// A zero time means nothing has ever been polled, which the caller reports as zero
// rather than as now: a fresh installation has not refreshed anything, and saying it
// refreshed this instant would be a small lie a client might plan around.
func (s *Store) LastRefreshedAt(ctx context.Context, userID UserID) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT max(last_polled_at) FROM feeds WHERE user_id = $1`, userID,
	).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("reading the last refresh time for user %d: %w", userID, err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}
