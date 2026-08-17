package server

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// pageData is what every page needs regardless of what it shows: who is reading,
// and enough to draw the chrome.
type pageData struct {
	User     store.UserID
	Username string
	Unread   int64

	// Nav marks the current section so the chrome can show where you are.
	Nav string
}

func (s *Server) pageData(r *http.Request, nav string) pageData {
	userID := signedInUser(r)

	d := pageData{User: userID, Username: s.cfg.Username, Nav: nav}

	// A failed count is not worth failing a page over — the reader came here to
	// read, not to see a number.
	if counts, err := s.store.UnreadCountsFor(r.Context(), userID); err != nil {
		s.log.Warn("counting unread failed", "error", err)
	} else {
		d.Unread = counts.Total
	}
	return d
}

// streamPage is the unread stream, the starred list, a feed, or a tag: the same
// view with a different title and filter.
type streamPage struct {
	pageData

	Heading  string
	Empty    string
	Items    []store.StreamItem
	NextPage string
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	s.serveStream(w, r, streamRequest{
		nav:     "unread",
		heading: "Unread",
		empty:   "Nothing unread. The worker fills this in as feeds are polled.",
		query:   store.StreamQuery{UnreadOnly: true},
		path:    "/",
	})
}

func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	s.serveStream(w, r, streamRequest{
		nav:     "all",
		heading: "Everything",
		empty:   "The archive is empty. Import feeds and run the worker.",
		path:    "/all",
	})
}

func (s *Server) handleStarred(w http.ResponseWriter, r *http.Request) {
	s.serveStream(w, r, streamRequest{
		nav:     "starred",
		heading: "Starred",
		empty:   "Nothing starred yet. Press s on an article, or use the button in the reader.",
		query:   store.StreamQuery{StarredOnly: true},
		path:    "/starred",
	})
}

func (s *Server) handleFeedStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	feed, err := s.store.GetFeed(r.Context(), signedInUser(r), store.FeedID(id))
	if err != nil {
		// Not found rather than forbidden, for the same reason as articles: a
		// distinct "forbidden" would confirm the feed exists.
		s.notFoundOrError(w, r, err, "reading a feed")
		return
	}

	s.serveStream(w, r, streamRequest{
		nav:     "feeds",
		heading: feed.Title,
		empty:   "Nothing stored from this feed yet.",
		query:   store.StreamQuery{FeedID: feed.ID},
		path:    "/feeds/" + strconv.FormatInt(int64(feed.ID), 10),
	})
}

func (s *Server) handleTagStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	s.serveStream(w, r, streamRequest{
		nav:     "tags",
		heading: "Tagged",
		empty:   "Nothing carries this tag.",
		query:   store.StreamQuery{TagID: store.TagID(id)},
		path:    "/tags/" + strconv.FormatInt(id, 10),
	})
}

type streamRequest struct {
	nav     string
	heading string
	empty   string
	query   store.StreamQuery
	path    string
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, req streamRequest) {
	userID := signedInUser(r)

	q := req.query
	// One more than the page size, so "is there a next page" is answered by the
	// query rather than by guessing from a full page — a page that happens to end
	// exactly on the boundary would otherwise offer a link to nothing.
	q.Limit = store.DefaultStreamLimit
	if before := r.URL.Query().Get("before"); before != "" {
		sortAt, id, ok := parseCursor(before)
		if !ok {
			http.Error(w, "that page marker could not be read", http.StatusBadRequest)
			return
		}
		q.BeforeSort, q.BeforeID = sortAt, id
	}
	q.Limit++

	items, err := s.store.Stream(r.Context(), userID, q)
	if err != nil {
		s.log.Error("listing the stream failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := streamPage{
		pageData: s.pageData(r, req.nav),
		Heading:  req.heading,
		Empty:    req.empty,
	}
	if len(items) > store.DefaultStreamLimit {
		last := items[store.DefaultStreamLimit-1]
		items = items[:store.DefaultStreamLimit]
		page.NextPage = req.path + "?before=" + formatCursor(last.SortAt, last.ArticleID)
	}
	page.Items = items

	// An htmx request for the next page wants rows, not a document.
	if isHTMX(r) && r.URL.Query().Get("before") != "" {
		s.renderFragment(w, http.StatusOK, "stream-rows", page)
		return
	}
	s.render(w, http.StatusOK, "stream", page)
}

// articlePage is the reader.
type articlePage struct {
	pageData

	Article store.Article
	Body    template.HTML
	HasBody bool
	Read    bool
	Starred bool
	Tags    []store.Tag
	Words   int

	// Notice explains why there is no body, when there is not one.
	Notice string
}

func (s *Server) handleArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	userID := signedInUser(r)

	view, err := s.store.ArticleForUser(r.Context(), userID, store.ArticleID(id))
	if err != nil {
		s.notFoundOrError(w, r, err, "reading an article")
		return
	}

	page := articlePage{
		pageData: s.pageData(r, ""),
		Article:  view.Article,
		HasBody:  view.HasBody,
		Read:     view.Read,
		Starred:  view.Starred,
		Tags:     view.Tags,
		Words:    view.Content.WordCount,
	}

	if view.HasBody {
		// Marked safe because extraction sanitized it with bluemonday before
		// it was ever stored, and the CSP on this response blocks script and
		// third-party requests regardless. Neither alone would be enough; together
		// they are why this is not reckless.
		page.Body = template.HTML(view.Content.HTML) //nolint:gosec // sanitized at extraction; see the extraction ladder
	} else {
		page.Notice = noticeFor(view.Article)
	}

	// Opening an article marks it read. That is what a reader expects, and the
	// alternative — an explicit button — means every article stays unread forever
	// for anyone who does not remember to press it.
	if !view.Read {
		if _, err := s.store.SetRead(r.Context(), userID, view.Article.ID, true); err != nil {
			// Not worth failing the page: the reader is here to read.
			s.log.Warn("marking an article read failed", "article_id", view.Article.ID, "error", err)
		} else {
			page.Read = true
			if page.Unread > 0 {
				page.Unread--
			}
		}
	}

	s.render(w, http.StatusOK, "article", page)
}

// noticeFor explains an article with no body, in the reader's terms.
func noticeFor(a store.Article) string {
	switch {
	case a.FetchStatus == store.FetchSkipped:
		return "This page was not fetched because the site's robots.txt asked us not to. " +
			"The original is still linked above."
	case a.FetchStatus == store.FetchFailed && a.FetchError != "":
		return "This page could not be archived: " + a.FetchError + "."
	case a.FetchStatus == store.FetchFailed:
		return "This page could not be archived."
	case a.FetchStatus == store.FetchPending:
		return "This page has not been fetched yet. The worker will pick it up."
	default:
		return "There is no stored body for this article."
	}
}

// searchPage is the results list.
type searchPage struct {
	pageData

	Query   string
	Results []store.SearchResult
	Ran     bool
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("q"))

	page := searchPage{pageData: s.pageData(r, "search"), Query: text, Ran: text != ""}

	if page.Ran {
		results, err := s.search.Query(r.Context(), signedInUser(r), store.SearchQuery{Text: text})
		if err != nil {
			s.log.Error("search failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		page.Results = results
	}

	s.render(w, http.StatusOK, "search", page)
}

// feedsPage is the feed list and its health.
type feedsPage struct {
	pageData

	Feeds  []feedRow
	Tags   []store.Tag
	Broken int
}

type feedRow struct {
	store.Feed
	Unread int64
}

func (s *Server) handleFeeds(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	feeds, err := s.store.ListFeeds(r.Context(), userID)
	if err != nil {
		s.log.Error("listing feeds failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	counts, err := s.store.UnreadCountsFor(r.Context(), userID)
	if err != nil {
		s.log.Error("counting unread failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tags, err := s.store.ListTags(r.Context(), userID)
	if err != nil {
		s.log.Error("listing tags failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := feedsPage{pageData: s.pageData(r, "feeds"), Tags: tags}
	page.Unread = counts.Total

	for _, f := range feeds {
		if f.ConsecutiveFailures > 0 || f.Disabled {
			page.Broken++
		}
		page.Feeds = append(page.Feeds, feedRow{Feed: f, Unread: counts.ByFeed[f.ID]})
	}

	s.render(w, http.StatusOK, "feeds", page)
}

// attentionPage is the failed-fetch queue.
type attentionPage struct {
	pageData
	Items []store.NeedsAttention
}

func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.NeedsAttentionFor(r.Context(), signedInUser(r), store.MaxStreamLimit)
	if err != nil {
		s.log.Error("listing articles needing attention failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.render(w, http.StatusOK, "attention", attentionPage{
		pageData: s.pageData(r, "attention"),
		Items:    items,
	})
}

// actions is the data behind the read/star controls, rendered both inside a page
// and alone as an htmx response.
type actions struct {
	ArticleID store.ArticleID
	Read      bool
	Starred   bool
}

func (s *Server) handleToggleRead(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, s.store.SetRead)
}

func (s *Server) handleToggleStar(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, s.store.SetStarred)
}

// stateSetter is the shape SetRead and SetStarred share, so toggle can be written
// once. Taking the context as a parameter rather than closing over the request is
// what keeps cancellation working through the indirection.
type stateSetter func(context.Context, store.UserID, store.ArticleID, bool) (bool, error)

// toggle applies a state change and returns the refreshed controls.
//
// The desired state comes from the form rather than being inferred by flipping
// what the server currently has: a double-tap or a retried request would otherwise
// toggle twice and land where it started.
func (s *Server) toggle(w http.ResponseWriter, r *http.Request, set stateSetter) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	on := r.PostFormValue("on") == "true"
	userID := signedInUser(r)

	written, err := set(r.Context(), userID, store.ArticleID(id), on)
	if err != nil {
		s.log.Error("updating article state failed", "article_id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !written {
		// The article is not visible to this reader. Not found, not forbidden.
		http.NotFound(w, r)
		return
	}

	view, err := s.store.ArticleForUser(r.Context(), userID, store.ArticleID(id))
	if err != nil {
		s.notFoundOrError(w, r, err, "re-reading an article after a state change")
		return
	}

	s.renderFragment(w, http.StatusOK, "actions", actions{
		ArticleID: view.Article.ID,
		Read:      view.Read,
		Starred:   view.Starred,
	})
}

// handleAsset serves an archived image.
//
// The stored body references assets as root-relative `/assets/...` so the reader
// can display them; the standalone `index.html` in the blob tree uses relative
// paths for the same files so it opens from disk with no server. Two path
// forms, one set of bytes.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		http.NotFound(w, r)
		return
	}

	// The blob store refuses anything escaping its root, so traversal is caught
	// there rather than by string inspection here. Still trimmed to a store-relative
	// path first, because that is what it expects.
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" || !strings.HasPrefix(path, "assets/") {
		http.NotFound(w, r)
		return
	}

	body, err := s.blobs.Get(r.Context(), path)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Warn("reading an asset failed", "path", path, "error", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = body.Close() }()

	// Content-addressed, so the bytes at a path never change: cache hard. This is
	// the one place in the application where an immutable cache is simply true
	// rather than a hopeful guess.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ct := contentTypeFor(path); ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	if _, err := io.Copy(w, body); err != nil {
		s.log.Debug("writing an asset failed", "path", path, "error", err)
	}
}

// contentTypeFor maps the extensions the asset pipeline produces.
//
// An explicit table rather than mime.TypeByExtension, which consults the host's
// /etc/mime.types and would therefore answer differently in a distroless container
// than on a developer's machine — and answers nothing at all for AVIF on many
// systems, which is the format most of the archive is in.
func contentTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".avif"):
		return "image/avif"
	case strings.HasSuffix(path, ".webp"):
		return "image/webp"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	default:
		return ""
	}
}

// notFoundOrError distinguishes "you cannot see this" from "something broke".
func (s *Server) notFoundOrError(w http.ResponseWriter, r *http.Request, err error, doing string) {
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	s.log.Error(doing+" failed", "path", r.URL.Path, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// The cursor is the sort timestamp and the id, which is exactly the keyset the
// store pages on. Opaque to the reader but deliberately not encrypted: it is a
// position in their own list, and a tampered value can only produce a different
// page of articles they can already see.
func formatCursor(at time.Time, id store.ArticleID) string {
	return strconv.FormatInt(at.UnixMicro(), 10) + "-" + strconv.FormatInt(int64(id), 10)
}

func parseCursor(s string) (time.Time, store.ArticleID, bool) {
	micro, rest, found := strings.Cut(s, "-")
	if !found {
		return time.Time{}, 0, false
	}
	at, err := strconv.ParseInt(micro, 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id < 0 {
		return time.Time{}, 0, false
	}
	return time.UnixMicro(at).UTC(), store.ArticleID(id), true
}
