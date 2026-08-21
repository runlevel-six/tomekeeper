package server

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/archive"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The Fever API: one endpoint that lets an existing mobile RSS client read this
// archive, so that goal 5 — work with the clients people already have rather than
// requiring a bespoke app — costs a protocol implementation instead of an app.
//
// # What the protocol is
//
// Fever was a hosted reader, discontinued around 2016, whose sync API a generation
// of iOS and Android clients implemented. The specification is frozen — its own
// text promises that existing features "will not be removed or modified" — so the
// 2016 wording is the final word on the wire format, and this implementation is
// written against it directly. What that document does *not* describe is what
// clients actually send, because it was written before most of them existed. Those
// details were settled by reading Miniflux's implementation, which is the server
// the surviving clients are tested against, and each one is called out below.
//
// # The shape
//
// One route, POST, form-encoded. Reads are named by query-string arguments
// (`?api&items`), writes by POST fields (`mark=item&as=read&id=7`), and the
// credential is `api_key` — MD5 of "username:password", which is not a choice made
// here but the format every client implements. See internal/auth for why that value
// has to be written when a password is set rather than derived later.
//
// # Deviations, all deliberate
//
//   - **Writes are handled before reads, and every requested member is answered.**
//     The specification says a mark response carries the updated id lists, so
//     clients legitimately combine a POSTed mark with query arguments; a server that
//     dispatched on the first match — as Miniflux does — would silently drop the
//     write and answer the read. Losing a read costs a retry, losing a write loses
//     state, so the write wins and the reads are additive on top of it.
//   - **`max_id=0` means the newest page, not "id below zero".** Taken literally
//     the specification's own initial-sync instruction returns nothing.
//   - **`as=unread` is accepted** though the specification lists only read, saved
//     and unsaved for `mark=item`. Clients send it and Miniflux honors it.
//   - **`before` is honored on `mark=group&id=0`**, where Miniflux ignores it. It
//     exists precisely so that a bulk mark cannot reach items the client has not
//     shown anybody, and there is no reason the whole-archive case should be the one
//     exception. A client that omits it marks the list as it stands, which is what
//     the web interface's own bulk mark does.
//   - **`links` and `favicons` return empty arrays.** Hot links is a computed
//     popularity ranking, which §1 puts out of scope; favicons are not stored.
//     Answering with an empty array rather than omitting the member keeps a client
//     that asks for them working.
//   - **`unread_recently_read` and `api=xml` are not implemented.**
//
// # The mappings that are this archive's own
//
// An item id is an article id, a group is a category, and is_saved is the starred
// flag. Each of those is a structural decision with a consequence, and each is
// argued where it is implemented — see internal/store/fever.go for the first and
// third, and feverGroupsFor below for the second.

// feverAPIVersion is the version this speaks. Three is what Fever 1.14 reported and
// what every surviving client expects.
const feverAPIVersion = 3

// feverPayload is one response.
//
// A map rather than a struct because the protocol's response is a base object plus
// whichever members were asked for, and expressing "present only if requested" with
// struct tags means either pointers everywhere or a struct per combination — while
// the combinations are what a client chooses at runtime.
type feverPayload map[string]any

// handleFever is the whole API.
//
// Deliberately not behind requireUser: this has its own credential, and a client
// presenting a valid api_key has no session cookie to offer. The authentication
// failure it returns is HTTP 200 with `auth: 0`, which looks wrong and is right —
// the protocol carries its own status inside the body, and clients read that rather
// than the HTTP code.
func (s *Server) handleFever(w http.ResponseWriter, r *http.Request) {
	// Reads the POST body, which is where the credential lives. A body that cannot
	// be parsed is reported as a failed authentication rather than a 400: there is
	// no api_key in it, and that is the same answer by a shorter route.
	if err := r.ParseForm(); err != nil {
		s.log.Warn("a fever request could not be read", "error", err, "remote", r.RemoteAddr)
		s.writeFever(w, feverPayload{"api_version": feverAPIVersion, "auth": 0})
		return
	}

	userID, ok := s.feverAuthenticate(r)
	if !ok {
		s.writeFever(w, feverPayload{"api_version": feverAPIVersion, "auth": 0})
		return
	}

	payload, err := s.feverAnswer(r.Context(), r, userID)
	if err != nil {
		s.log.Error("a fever request failed", "error", err, "user_id", userID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeFever(w, payload)
}

// feverAuthenticate resolves the api_key a request presents.
//
// FormValue rather than PostFormValue, so a client that puts the key in the query
// string is not turned away over a detail the specification states but nothing
// enforces. The failure is logged once per attempt and never quotes the key.
func (s *Server) feverAuthenticate(r *http.Request) (store.UserID, bool) {
	apiKey := r.FormValue("api_key")
	if apiKey == "" {
		s.log.Warn("a fever request arrived with no api key", "remote", r.RemoteAddr,
			"user_agent", r.UserAgent())
		return 0, false
	}

	userID, err := s.store.System().UserByAPIKey(r.Context(), apiKey)
	if err != nil {
		// Not distinguished from "no such key" in the response, and only barely in
		// the log: a client learning which of the two applied would learn whether an
		// account exists.
		s.log.Warn("a fever api key was not accepted", "remote", r.RemoteAddr,
			"user_agent", r.UserAgent(), "error", err)
		return 0, false
	}
	return userID, true
}

// writeFever encodes one response.
func (s *Server) writeFever(w http.ResponseWriter, payload feverPayload) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A sync response is state as of this instant. Nothing about it may be reused,
	// least of all by a proxy in front of somebody's phone.
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status is already sent, so this cannot become a 500. Debug because a
		// client that hung up mid-response is ordinary on a mobile network.
		s.log.Debug("writing a fever response failed", "error", err)
	}
}

// feverAnswer builds the response for one authenticated request.
func (s *Server) feverAnswer(ctx context.Context, r *http.Request, userID store.UserID) (feverPayload, error) {
	payload := feverPayload{"api_version": feverAPIVersion, "auth": 1}

	refreshed, err := s.store.LastRefreshedAt(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Zero when nothing has ever been polled, rather than now(): a fresh
	// installation has not refreshed anything, and claiming otherwise is a statement
	// a client may plan its next sync around.
	var refreshedUnix int64
	if !refreshed.IsZero() {
		refreshedUnix = refreshed.Unix()
	}
	payload["last_refreshed_on_time"] = refreshedUnix

	// Writes first. See the deviation note at the top of this file for why this is
	// not the order Miniflux uses.
	wrote, err := s.feverWrite(ctx, r, userID)
	if err != nil {
		return nil, err
	}

	query := r.URL.Query()

	if query.Has("groups") || query.Has("feeds") {
		feeds, err := s.store.ListFeeds(ctx, userID)
		if err != nil {
			return nil, err
		}
		groups, byGroup := feverGroupsFor(feeds)

		if query.Has("groups") {
			payload["groups"] = groups
		}
		if query.Has("feeds") {
			payload["feeds"] = feverFeeds(feeds)
		}
		// Carried by both requests, per the protocol.
		payload["feeds_groups"] = byGroup
	}

	if query.Has("favicons") {
		payload["favicons"] = []any{}
	}
	if query.Has("links") {
		payload["links"] = []any{}
	}

	if query.Has("items") {
		items, total, err := s.feverItems(ctx, r, userID, query)
		if err != nil {
			return nil, err
		}
		payload["items"] = items
		payload["total_items"] = total
	}

	// A mark response carries the updated id lists whether or not they were asked
	// for, which is what the specification means by returning them "as appropriate".
	// It is also what makes a client's cache correct after a write it did not follow
	// with a read.
	if query.Has("unread_item_ids") || wrote {
		ids, err := s.store.UnreadArticleIDs(ctx, userID)
		if err != nil {
			return nil, err
		}
		payload["unread_item_ids"] = feverIDList(ids)
	}
	if query.Has("saved_item_ids") || wrote {
		ids, err := s.store.StarredArticleIDs(ctx, userID)
		if err != nil {
			return nil, err
		}
		payload["saved_item_ids"] = feverIDList(ids)
	}

	return payload, nil
}

// feverWrite applies a mark request, reporting whether one was present.
func (s *Server) feverWrite(ctx context.Context, r *http.Request, userID store.UserID) (bool, error) {
	// FormValue, so a mark in the query string works too. Only the POST body is
	// specified, but nothing is lost by accepting both and a client that got it
	// wrong would otherwise fail silently.
	switch r.FormValue("mark") {
	case "item":
		return true, s.feverMarkItem(ctx, r, userID)
	case "feed":
		return true, s.feverMarkFeed(ctx, r, userID)
	case "group":
		return true, s.feverMarkGroup(ctx, r, userID)
	default:
		return false, nil
	}
}

// feverMarkItem applies mark=item.
//
// An id that names an article the reader cannot see writes nothing, because the
// store's setters bound themselves by visibility and report having written no row.
// That is reported as success, which is the protocol's own behavior for an unknown
// item — and the alternative would let a client discover which article ids exist in
// somebody else's archive by watching for a different answer.
func (s *Server) feverMarkItem(ctx context.Context, r *http.Request, userID store.UserID) error {
	id := store.ArticleID(feverInt(r, "id"))
	if id <= 0 {
		return nil
	}

	var (
		wrote bool
		err   error
		as    = r.FormValue("as")
	)
	switch as {
	case "read":
		wrote, err = s.store.SetRead(ctx, userID, id, true)
	case "unread":
		// Not in the specification's list for mark=item, but clients send it and it
		// is the only way one can undo a mistaken tap.
		wrote, err = s.store.SetRead(ctx, userID, id, false)
	case "saved":
		wrote, err = s.store.SetStarred(ctx, userID, id, true)
	case "unsaved":
		wrote, err = s.store.SetStarred(ctx, userID, id, false)
	default:
		// An unrecognized `as` is not an error the protocol can express. Logged so
		// that a client doing something unexpected is discoverable rather than
		// mysterious.
		s.log.Warn("a fever client asked to mark an item in an unknown way",
			"as", as, "article_id", id, "user_id", userID)
		return nil
	}
	if err != nil {
		return err
	}

	s.log.Debug("fever marked an item", "as", as, "article_id", id,
		"user_id", userID, "changed", wrote)
	return nil
}

// feverMarkFeed applies mark=feed.
//
// No check that the feed belongs to this reader, deliberately: the filter inside
// StreamQuery carries its own `user_id`, so another reader's feed id selects nothing
// at all. A check here would be a second place for the same rule to be right, and
// the store's version is the one that cannot be bypassed.
func (s *Server) feverMarkFeed(ctx context.Context, r *http.Request, userID store.UserID) error {
	feedID := store.FeedID(feverInt(r, "id"))
	if feedID <= 0 {
		return nil
	}

	n, err := s.store.MarkReadIn(ctx, userID, store.StreamQuery{
		FeedID:       feedID,
		SortedBefore: feverBefore(r),
	})
	if err != nil {
		return err
	}

	s.log.Info("fever marked a feed read", "feed_id", feedID, "user_id", userID, "articles", n)
	return nil
}

// feverMarkGroup applies mark=group, including the two super groups.
func (s *Server) feverMarkGroup(ctx context.Context, r *http.Request, userID store.UserID) error {
	groupID := feverInt(r, "id")

	// Negative ids are the "Sparks" super group, which is every feed flagged as
	// low-priority. Nothing here is a spark — see feverFeeds — so this is a genuine
	// no-op rather than an unimplemented case.
	if groupID < 0 {
		s.log.Debug("fever asked to mark the sparks group read, which is always empty here",
			"group_id", groupID, "user_id", userID)
		return nil
	}

	q := store.StreamQuery{SortedBefore: feverBefore(r)}
	what := "everything"

	if groupID > 0 {
		// Resolved from the reader's own subscriptions, which is where the groups
		// response takes its ids from too — so the two cannot disagree about which
		// folder an id means. It used to need the *names*, because the id was a hash
		// of one.
		feeds, err := s.store.ListFeeds(ctx, userID)
		if err != nil {
			return err
		}
		category, ok := feverCategoryFor(feeds, groupID)
		if !ok {
			// A group id this reader has no category for. Nothing to mark, and worth a
			// line: it is what a stale client cache looks like.
			s.log.Warn("fever asked to mark an unknown group read",
				"group_id", groupID, "user_id", userID)
			return nil
		}
		q.Categorized, q.Category = true, category
		what = "category " + strconv.Quote(category)
	}

	// Note what an id of zero means: an otherwise empty StreamQuery, which marks
	// everything the reader can see. That is Fever's "Kindling" super group and it is
	// the one place in this application where a bulk mark is meant to reach the whole
	// archive — so it is logged at info with its count, rather than at debug like the
	// per-item marks.
	n, err := s.store.MarkReadIn(ctx, userID, q)
	if err != nil {
		return err
	}

	s.log.Info("fever marked a group read", "group", what, "group_id", groupID,
		"user_id", userID, "articles", n)
	return nil
}

// feverItems answers the items request.
func (s *Server) feverItems(ctx context.Context, r *http.Request, userID store.UserID,
	query url.Values,
) ([]feverItem, int64, error) {
	var q store.FeverItemQuery
	switch {
	case query.Has("with_ids"):
		q.IDs = feverIDs(query.Get("with_ids"))
		if len(q.IDs) == 0 {
			// An empty or unparseable list is not the whole archive. Answering with
			// nothing is the only reading that cannot surprise anybody.
			return []feverItem{}, 0, nil
		}
	case query.Has("since_id"):
		q.SinceID = store.ArticleID(feverQueryInt(query, "since_id"))
	case query.Has("max_id"):
		// Zero is the documented value for "I have nothing cached", so it means the
		// newest page rather than an upper bound of zero.
		if maxID := feverQueryInt(query, "max_id"); maxID > 0 {
			q.MaxID = store.ArticleID(maxID)
		} else {
			q.Newest = true
		}
	}

	items, err := s.store.FeverItems(ctx, userID, q)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.store.CountVisibleArticles(ctx, userID)
	if err != nil {
		return nil, 0, err
	}

	out := make([]feverItem, 0, len(items))
	for _, it := range items {
		out = append(out, feverItem{
			ID:        int64(it.ArticleID),
			FeedID:    int64(it.FeedID),
			Title:     it.Title,
			Author:    it.Author,
			HTML:      s.feverBody(r, it),
			URL:       it.URL,
			IsSaved:   feverBool(it.Starred),
			IsRead:    feverBool(it.Read),
			CreatedAt: it.CreatedAt.Unix(),
		})
	}
	return out, total, nil
}

// feverBody is the article body as a client will render it.
//
// Two transformations, and both exist because the client is not a browser sitting
// on an authenticated session.
//
// Images first. A stored body references them as "/assets/sha256/…", which is
// meaningless to a client that has no base URL for this service, and `/assets/` has
// always required a session — so left alone, every picture in every client is a
// broken image icon. They become absolute and signed: the signature is the
// credential, which is the same answer Miniflux reaches with its media proxy. See
// internal/asseturl.
//
// Then the empty case. An article whose extraction produced nothing, or whose body
// retention has released, has no HTML at all, and a client showing a blank pane
// cannot say which of those happened or offer a way onward. One sentence and a link
// is not the archive inventing content: it is the same thing the reader's own page
// says with a badge, in the only field this protocol has to say it in.
func (s *Server) feverBody(r *http.Request, it store.FeverItem) string {
	if it.HTML == "" {
		return feverMissingBody(it.URL)
	}
	if s.assetURLs == nil {
		// No signer, so a signed URL is not available. The body still goes out; its
		// pictures do not resolve, which is strictly better than withholding the text.
		return it.HTML
	}

	base := feverBaseURL(r, s.cfg.CookieSecure)
	return archive.MapAssetRefs(it.HTML, func(ref string) string {
		return base + s.assetURLs.Sign(ref)
	})
}

// feverMissingBody is the one sentence that stands in for an article the archive has
// no copy of.
//
// Only an http or https URL becomes a link. Canonicalization produces nothing else,
// so this is belt and braces — but the thing being guarded against is emitting a
// scheme this application did not choose into an href that a mobile client will
// render, and the guard costs two lines.
func feverMissingBody(rawURL string) string {
	const sentence = `<p><em>This archive has no stored copy of this article.</em>`

	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return sentence + `</p>`
	}
	return sentence + ` <a href="` + template.HTMLEscapeString(rawURL) + `">Open the original</a></p>`
}

// feverBaseURL is the absolute origin to hang asset URLs off.
//
// The host is the one the client asked for, because that is by definition a name
// that reaches this service from where the client is standing; a configured base URL
// would be a second source of truth that is wrong the first time somebody adds a
// hostname.
//
// The scheme cannot be read from the request when TLS is terminated upstream, which
// is every deployment behind an Ingress. Rather than trust an X-Forwarded-Proto
// header, it falls back to what the deployment already had to declare about itself:
// COOKIE_SECURE, whose existing meaning is "this service is reached over HTTPS".
func feverBaseURL(r *http.Request, secure bool) string {
	scheme := "http"
	if r.TLS != nil || secure {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// feverItem is one item on the wire. Field names and types are the protocol's.
type feverItem struct {
	ID        int64  `json:"id"`
	FeedID    int64  `json:"feed_id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	HTML      string `json:"html"`
	URL       string `json:"url"`
	IsSaved   int    `json:"is_saved"`
	IsRead    int    `json:"is_read"`
	CreatedAt int64  `json:"created_on_time"`
}

// feverFeed is one feed on the wire.
type feverFeed struct {
	ID        int64  `json:"id"`
	FaviconID int64  `json:"favicon_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	SiteURL   string `json:"site_url"`

	// IsSpark is always zero. Sparks were Fever's low-priority feeds, a distinction
	// this archive does not make and would have to invent a column to make.
	IsSpark int `json:"is_spark"`

	// LastUpdated is when the feed last actually had something to give, which is what
	// "updated" means as against the response-level last_refreshed_on_time. Zero for
	// a feed that has never succeeded, including every feed in the failing list.
	LastUpdated int64 `json:"last_updated_on_time"`
}

// feverGroup is one group on the wire.
type feverGroup struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// feverFeedsGroup is one group's membership, as the protocol spells it.
type feverFeedsGroup struct {
	GroupID int64 `json:"group_id"`

	// FeedIDs is a comma-separated list in a string, which is the protocol's own
	// choice rather than this one's.
	FeedIDs string `json:"feed_ids"`
}

// feverFeeds renders the subscription list.
//
// Disabled feeds are included. A feed that stopped being polled still has articles
// in the archive, and a client that could not see the feed would show those articles
// as belonging to nothing — so the honest thing is to list it with a last-updated
// time that says how long it has been quiet.
func feverFeeds(feeds []store.Feed) []feverFeed {
	out := make([]feverFeed, 0, len(feeds))
	for _, f := range feeds {
		var updated int64
		if f.LastSuccessAt != nil {
			updated = f.LastSuccessAt.Unix()
		}
		out = append(out, feverFeed{
			ID:    int64(f.ID),
			Title: f.Title,
			URL:   f.FeedURL,
			// The protocol has favicon_id as a positive integer and zero as "none",
			// which is every feed here: favicons are not stored, and the favicons
			// request answers with an empty array to match.
			FaviconID:   0,
			SiteURL:     f.SiteURL,
			IsSpark:     0,
			LastUpdated: updated,
		})
	}
	return out
}

// feverGroupsFor builds the group list and the memberships from one pass over the
// subscriptions.
//
// Feeds with no category appear in no group. Fever has no concept of an ungrouped
// feed and clients cope with it — they show such items under "All Items" — whereas
// inventing an "Uncategorized" group would put a folder in somebody's reader that
// does not exist in their archive.
func feverGroupsFor(feeds []store.Feed) ([]feverGroup, []feverFeedsGroup) {
	// The id comes from the categories row, not from the name. It used to be a hash
	// of the name — there was no table and the protocol requires an id — and the
	// consequence was that **renaming a category silently reshuffled a client's
	// folders**, because clients cache folder membership against these ids. The old
	// folder vanished and a new one appeared holding the same feeds. Migration 00013
	// exists for this.
	names := make([]string, 0, len(feeds))
	ids := make(map[string]int64, len(feeds))
	for _, f := range feeds {
		if f.Category == "" || f.CategoryID == 0 {
			continue
		}
		if !slices.Contains(names, f.Category) {
			names = append(names, f.Category)
			ids[f.Category] = int64(f.CategoryID)
		}
	}

	groups := make([]feverGroup, 0, len(names))
	for _, name := range names {
		groups = append(groups, feverGroup{ID: ids[name], Title: name})
	}
	// Sorted by title so a client's folder list has a stable order rather than
	// whatever order the subscriptions came back in.
	sort.Slice(groups, func(i, j int) bool { return groups[i].Title < groups[j].Title })

	members := make(map[int64][]string, len(names))
	for _, f := range feeds {
		if f.Category == "" {
			continue
		}
		id := ids[f.Category]
		members[id] = append(members[id], strconv.FormatInt(int64(f.ID), 10))
	}

	byGroup := make([]feverFeedsGroup, 0, len(members))
	for _, g := range groups {
		if list, ok := members[g.ID]; ok {
			byGroup = append(byGroup, feverFeedsGroup{GroupID: g.ID, FeedIDs: strings.Join(list, ",")})
		}
	}
	return groups, byGroup
}

// feverCategoryFor is the category a group id names.
//
// Resolved against the reader's own subscriptions, so a group id belonging to another
// reader's folder resolves to nothing here — the categories table is scoped per
// reader, and this only ever sees one reader's feeds.
//
// The nameless bucket is not a group and must not be assignable an id: the groups
// response leaves it out, and a group id resolving to "no category" would mark the
// wrong articles read.
func feverCategoryFor(feeds []store.Feed, groupID int64) (string, bool) {
	for _, f := range feeds {
		if f.Category != "" && f.CategoryID != 0 && int64(f.CategoryID) == groupID {
			return f.Category, true
		}
	}
	return "", false
}

// feverIDList renders an id list the way the protocol carries them: decimal, comma
// separated, in one string.
func feverIDList(ids []store.ArticleID) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(int64(id), 10)
	}
	return strings.Join(parts, ",")
}

// feverIDs parses a with_ids list.
//
// Junk entries are skipped rather than failing the request, which is the opposite of
// how the scroll-marking endpoint treats a malformed list — and for the opposite
// reason. That one writes, so a partial application is a wrong number of articles
// marked read; this one reads, and a client that asked for fifty items and named one
// badly is better served with forty-nine than with nothing.
func feverIDs(raw string) []store.ArticleID {
	var ids []store.ArticleID
	for _, field := range strings.Split(raw, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		ids = append(ids, store.ArticleID(n))
		if len(ids) == store.FeverItemLimit {
			// The protocol caps with_ids at fifty. The store caps it too; stopping here
			// keeps the request from silently meaning something other than it says.
			break
		}
	}
	return ids
}

// feverBefore reads the `before` guard on a bulk mark.
//
// Absent, zero or unparseable means no bound, which marks the list as it stands.
// That is what the web interface's own bulk mark does, and it is the reading that
// makes a client's "mark all read" button work rather than silently doing nothing —
// which is what treating a missing timestamp as the epoch would produce.
func feverBefore(r *http.Request) time.Time {
	if before := feverInt(r, "before"); before > 0 {
		return time.Unix(before, 0)
	}
	return time.Time{}
}

// feverInt reads an integer from the form, which covers the POST body and the query.
func feverInt(r *http.Request, name string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// feverQueryInt reads an integer from the query string, which is where the protocol
// puts the read arguments — as against feverInt, which also reads the POST body
// because that is where it puts the write arguments.
func feverQueryInt(query url.Values, name string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(query.Get(name)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// feverBool renders the protocol's "boolean integer".
func feverBool(b bool) int {
	if b {
		return 1
	}
	return 0
}
