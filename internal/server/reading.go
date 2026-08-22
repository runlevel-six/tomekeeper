package server

import (
	"context"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
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

	// IsAdmin says whether to offer the controls that change what everyone
	// shares. The chrome hides them; requireAdmin is what actually refuses them,
	// and a hidden link is a courtesy rather than a boundary.
	IsAdmin bool

	// Nav marks the current section so the chrome can show where you are.
	Nav string

	// Path is this page's own URL, which is what the reload control links to.
	//
	// A link rather than a scripted reload, because installed as a web app there
	// is no browser reload button and a reader with JavaScript off should still be
	// able to ask for fresh contents.
	Path string

	// Theme is the value for <html data-theme>. Rendered into every page rather
	// than applied by a script, so the palette is right in the first paint.
	Theme string

	// TextScale is the value for <html data-text>. Rendered for the same reason as
	// the palette and a more pressing one: a size applied by script reflows the
	// page after it has been laid out, which is a worse experience than either
	// size on its own.
	TextScale string

	// MarkReadOnScroll is the reader's preference, not a decision about this page:
	// which lists act on it is the list's own business. Off unless they turned it
	// on.
	MarkReadOnScroll bool

	// DefaultPollInterval is their general feed-checking cadence, nil for
	// automatic. Carried here because two pages need it and the preferences row is
	// already being read; nothing in the chrome draws it.
	DefaultPollInterval *time.Duration
}

func (s *Server) pageData(r *http.Request, nav string) pageData {
	account := signedInAccount(r)
	userID := account.ID

	// The name and the role come from the account this request is authenticated
	// as, not from configuration. TOME_USERNAME names the account `tome migrate`
	// seeds and nothing else, so reading it here showed every reader the same name
	// — invisible while there was one account and wrong the moment there were two.
	d := pageData{
		User: userID, Username: account.Username, IsAdmin: account.IsAdmin(),
		Nav: nav, Path: selfPath(r),
	}

	// A failed lookup costs the reader their palette for one page, which is a
	// far better outcome than costing them the page. Automatic marking falls back
	// to off for the same reason it defaults to off: a preference that fails open
	// would change state on a page that could not read the preference.
	if prefs, err := s.store.GetPreferences(r.Context(), userID); err != nil {
		s.log.Warn("reading preferences failed", "error", err)
	} else {
		d.Theme = prefs.Theme
		d.TextScale = prefs.TextScale
		d.MarkReadOnScroll = prefs.MarkReadOnScroll
		d.DefaultPollInterval = prefs.DefaultPollInterval
	}

	// A failed count is not worth failing a page over — the reader came here to
	// read, not to see a number.
	if counts, err := s.store.UnreadCountsFor(r.Context(), userID); err != nil {
		s.log.Warn("counting unread failed", "error", err)
	} else {
		d.Unread = counts.Total
	}
	return d
}

// streamPage is the unread stream, the starred list, a feed, a category, or a
// tag: the same view with a different title and filter.
type streamPage struct {
	pageData

	Heading  string
	Empty    string
	Items    []store.StreamItem
	NextPage string

	// From is the token every article link in this list carries, so that the
	// article page knows which list it was opened from.
	From string

	// Categories is the control that narrows this list to one category, empty on the
	// lists that do not take one and on an archive with nothing to choose between.
	Categories []categoryPill

	// Mark is the state of this list's mark-all-read control.
	Mark markControl

	// MarkOnScroll turns on the script that marks rows read as they go past: the
	// reader's preference and this list's own willingness, resolved here rather than
	// in the template so that the two halves cannot be checked in one place and
	// forgotten in another.
	MarkOnScroll bool

	// AtEnd says this render reaches the end of the list, so it is the one that
	// draws the end-of-list controls.
	//
	// True on the last page whether it arrived as a document or as the final
	// appended fragment, which is the only reason a control can sit at the bottom
	// of an infinitely-scrolling list at all.
	AtEnd bool
}

// markControl is the "mark all as read" control on a stream page: whether to
// offer it, what it would do, and what it did.
type markControl struct {
	// Offered is whether to draw the control at all. False for a list that cannot
	// be marked in bulk, and for one with nothing unread in it.
	Offered bool

	// Unread is how many articles the control would mark, which is the number it
	// puts in front of the reader before doing it. Counted over the whole list
	// rather than the page on screen, because that is what would be marked.
	Unread int64

	// Confirming is set on the page that asks. A bulk mark is the one control here
	// that cannot be undone in bulk — every other button on a stream row is its own
	// inverse — so it is two steps rather than one, and the count is what makes the
	// second step an informed one.
	Confirming bool

	// Done reports a mark that has just happened, and is nil otherwise.
	Done *markOutcome

	// From is the list's token, which is how the confirm and the POST name the list
	// they mean. Path is the list itself, for the cancel link.
	From string
	Path string
}

// markOutcome is what a stream page says after a bulk mark.
type markOutcome struct {
	Count   int64
	Problem string
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	// `?category=` narrows the unread list to one folder. Present-but-empty selects
	// the feeds that carry no category — a real bucket — which is why this tests for
	// the parameter rather than for a non-empty value, the same convention
	// /categories has used since it was written.
	if r.URL.Query().Has("category") {
		s.serveStream(w, r, s.unreadCategorySpec(r.URL.Query().Get("category")))
		return
	}

	s.serveStream(w, r, s.unreadSpec())
}

func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	// Everything narrowed to one category is a list that already has an address, so
	// this sends the reader to it rather than rendering the same articles at a second
	// one — two URLs for one list is how a Next button ends up computed from a
	// different definition than the list it belongs to. The control links there
	// directly; this is for the parameter typed by hand, which would otherwise look
	// ignored.
	if r.URL.Query().Has("category") {
		http.Redirect(w, r, categoryPath(r.URL.Query().Get("category")), http.StatusSeeOther)
		return
	}

	s.serveStream(w, r, s.allSpec())
}

func (s *Server) handleStarred(w http.ResponseWriter, r *http.Request) {
	s.serveStream(w, r, s.starredSpec())
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

	s.serveStream(w, r, s.feedSpec(feed))
}

func (s *Server) handleTagStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}

	s.serveStream(w, r, s.tagSpec(store.TagID(id)))
}

func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, spec streamSpec) {
	s.renderStream(w, r, spec, http.StatusOK, markControl{})
}

// renderStream draws one list, optionally asking about or reporting on a bulk
// mark.
//
// Split out of serveStream for the same reason renderFeeds was: the result of a
// POST belongs on the page that produced it, freshly counted. A mark that clears
// 247 articles has to leave behind a list that no longer contains them and a
// chrome badge that no longer counts them, and only a re-render can do that.
func (s *Server) renderStream(w http.ResponseWriter, r *http.Request, spec streamSpec,
	status int, mark markControl,
) {
	userID := signedInUser(r)

	q := spec.Query
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
		pageData: s.pageData(r, spec.Nav),
		Heading:  spec.Heading,
		Empty:    spec.Empty,
		From:     spec.Token,
		Mark:     mark,
	}
	if len(items) > store.DefaultStreamLimit {
		last := items[store.DefaultStreamLimit-1]
		items = items[:store.DefaultStreamLimit]
		page.NextPage = pageURL(spec.Path, "before", formatCursor(last.SortAt, last.ArticleID))
	}
	page.Items = items

	fragment := isHTMX(r) && r.URL.Query().Get("before") != ""

	// AtEnd is what makes the end of a list a place rather than an accident. With
	// rows appended as they are revealed, the last screenful is where a reader has
	// finished — and it is the one place the mark-read control was not, because it
	// sits in the page head above forty pages of articles.
	page.AtEnd = page.NextPage == ""

	page.Mark.From, page.Mark.Path = spec.Token, spec.Path
	if spec.Markable && (!fragment || page.AtEnd) {
		// Counted on the document and on the *final* fragment only. Keeping this
		// below the htmx return was about forty pages not paying for forty counts,
		// and one page in forty is not forty: the end of the list is the only
		// fragment that draws a control needing a number.
		//
		// A failed count costs the reader one control rather than the page they came
		// for, exactly as a failed unread tally does.
		if n, err := s.store.CountUnreadIn(r.Context(), userID, spec.Query); err != nil {
			s.log.Warn("counting unread in a stream failed", "from", spec.Token, "error", err)
		} else {
			page.Mark.Unread = n
			page.Mark.Offered = n > 0
		}
	}

	// An htmx request for the next page wants rows, not a document.
	if fragment {
		s.renderFragment(w, http.StatusOK, "stream-rows", page)
		return
	}

	// Below the htmx return above, deliberately: the control belongs to the document,
	// and a reader scrolling through forty pages of articles should not pay for it
	// forty times.
	page.Categories = s.categoryPills(r.Context(), userID, spec)

	// Same reasoning, and the same place: the attribute sits on the list's
	// container, which a fragment of rows does not redraw.
	page.MarkOnScroll = spec.ScrollMarkable && page.MarkReadOnScroll

	// The mark requests live at their own path, and pageData took this request's
	// path for the reload control — which would leave the reload button on a
	// /mark-read URL, pointing at a confirmation rather than at the list. The list
	// is what a reader means by reloading here.
	if mark.Confirming || mark.Done != nil {
		page.Path = spec.Path
	}

	s.render(w, status, "stream", page)
}

// handleMarkReadConfirm asks before marking a whole list read.
//
// A GET, and a page rather than a scripted dialog: the content security policy has
// no 'unsafe-inline' and this interface does not require JavaScript for anything a
// reader cannot otherwise do. It also means the question is reloadable and
// linkable, and answering "no" is navigating away — which is the cheapest possible
// cancel.
func (s *Server) handleMarkReadConfirm(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.markableSpec(w, r, r.URL.Query().Get("from"))
	if !ok {
		return
	}

	s.renderStream(w, r, spec, http.StatusOK, markControl{Confirming: true})
}

// handleMarkRead marks everything unread in one list read.
//
// Rendered rather than redirected, like the import and the on-demand poll: the
// count belongs to the request that earned it. Re-posting is harmless — the second
// one finds nothing unread and says so.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	spec, ok := s.markableSpec(w, r, r.PostFormValue("from"))
	if !ok {
		return
	}

	n, err := s.store.MarkReadIn(r.Context(), signedInUser(r), spec.Query)
	if err != nil {
		s.log.Error("marking a list read failed", "from", spec.Token, "error", err)
		s.renderStream(w, r, spec, http.StatusInternalServerError, markControl{
			Done: &markOutcome{Problem: "Nothing was marked read. The log will say why."},
		})
		return
	}

	s.log.Info("marked a list read", "from", spec.Token, "count", n)

	s.renderStream(w, r, spec, http.StatusOK, markControl{Done: &markOutcome{Count: n}})
}

// markableSpec resolves the list a mark-read request names.
//
// Not found covers three cases on purpose: a token that means nothing, a feed
// belonging to somebody else — GetFeed already reports that as missing rather than
// forbidden — and a list that exists but must not be marked in bulk. A reader who
// hand-crafts `from=search:x` gets the same nothing as one who invents a feed id,
// and neither learns anything from the difference.
func (s *Server) markableSpec(w http.ResponseWriter, r *http.Request, token string) (streamSpec, bool) {
	spec, ok := s.streamSpecFor(r.Context(), signedInUser(r), token)
	if !ok || !spec.Markable {
		http.NotFound(w, r)
		return streamSpec{}, false
	}
	return spec, true
}

// articlePage is the reader.
type articlePage struct {
	pageData

	Article store.Article
	Body    template.HTML
	HasBody bool
	Read    bool
	Starred bool
	Kept    bool
	Tags    []store.Tag
	Words   int

	// Notice explains why there is no body, when there is not one.
	Notice string

	// ImageNotice explains why a body's images are not showing, when they are
	// not. Separate from Notice because it accompanies a body rather than
	// replacing one.
	ImageNotice string

	// From, BackTo and BackLabel are the list this article was opened from.
	//
	// Installed as a web app there is no browser back button, so a way back to the
	// list is not a convenience — without it the only way out of an article is to
	// pick a section from the chrome and lose your place. BackTo always points
	// somewhere: an article opened from a bare link falls back to the unread list.
	From      string
	BackTo    string
	BackLabel string

	// Newer and Older are the articles either side of this one in that list, zero
	// where there is nothing there. Named for direction rather than
	// previous/next, which are ambiguous about whether they mean the list or the
	// clock; the templates say "Previous" and "Next" because that is what a reader
	// expects to read.
	Newer store.ArticleID
	Older store.ArticleID

	// Highlights are the passages this reader marked, in the order they were made.
	// Empty for almost every article: nothing in the interface creates one yet, and
	// the ones that exist arrived with an imported library.
	Highlights []store.ImportHighlight

	// Bodies are the other stored copies of this page, when there is more than one.
	// Empty in the ordinary case, which is most articles: one page, one body.
	Bodies []bodyChoice

	// Promoted is set when this render follows a reader choosing a different body,
	// so the page can say what it now shows rather than silently looking different.
	Promoted string
}

// bodyChoice is one stored body, as the reader chooses between them.
type bodyChoice struct {
	ID          store.ContentID
	Description string
	WordCount   int
	ExtractedAt time.Time
	Excerpt     string
	Current     bool
	Immutable   bool
}

// describeBody says where a body came from, in a reader's terms.
//
// The origin rather than the extractor name is what answers the question somebody
// is actually asking — is this the copy my old reader had, or the one this archive
// made? — so the extractor is the detail and the provenance is the sentence.
func describeBody(b store.StoredBody) string {
	switch {
	case strings.HasPrefix(b.ContentOrigin, "import:"):
		return "imported from " + strings.TrimPrefix(b.ContentOrigin, "import:")
	case b.ContentOrigin == store.OriginFeedBody:
		return "the summary the feed carried"
	case b.ExtractorName != "":
		version := b.ExtractorVersion
		if version != "" {
			version = " " + version
		}
		return "extracted from the stored page by " + b.ExtractorName + version
	default:
		return "an earlier body"
	}
}

// neighborReadWindow is how long an article stays in the unread list, for the
// purpose of working out what comes before and after it.
//
// Opening an article marks it read, so without this the unread list would
// rearrange itself under a reader the instant they started reading, and
// "previous" would point off the top of a list that no longer held anything they
// had seen. Half an hour is longer than any single article takes to read and
// shorter than a session someone comes back to the next day.
const neighborReadWindow = 30 * time.Minute

func (s *Server) handleArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveArticle(w, r, store.ArticleID(id), "")
}

// serveArticle renders the reader, optionally saying which body it now shows.
func (s *Server) serveArticle(w http.ResponseWriter, r *http.Request, id store.ArticleID, promoted string) {
	userID := signedInUser(r)

	view, err := s.store.ArticleForUser(r.Context(), userID, id)
	if err != nil {
		s.notFoundOrError(w, r, err, "reading an article")
		return
	}

	// Which list this was opened from. Unrecognized or absent leaves the reader
	// with a way back to the unread list and no previous/next, which is the right
	// outcome for a bare link.
	spec, haveList := s.streamSpecFor(r.Context(), userID, r.URL.Query().Get("from"))

	page := articlePage{
		pageData:  s.pageData(r, spec.Nav),
		Article:   view.Article,
		HasBody:   view.HasBody,
		Read:      view.Read,
		Starred:   view.Starred,
		Kept:      view.Kept,
		Tags:      view.Tags,
		Words:     view.Content.WordCount,
		BackTo:    "/",
		BackLabel: "Unread",
		Promoted:  promoted,
	}
	if haveList {
		page.From = spec.Token
		page.BackTo = spec.Path
		page.BackLabel = spec.Heading
	}

	if haveList && spec.Ordered {
		q := spec.Query
		q.ReadWithin = neighborReadWindow

		neighbors, err := s.store.NeighborsIn(r.Context(), userID, q, view.Article.ID)
		if err != nil {
			// Losing two links is not worth losing the article over.
			s.log.Warn("finding neighboring articles failed",
				"article_id", view.Article.ID, "from", spec.Token, "error", err)
		} else {
			page.Newer, page.Older = neighbors.Newer, neighbors.Older
		}
	}

	// The passages this reader marked. Shown because they exist and are otherwise
	// invisible: an imported library can carry years of them, and an archive that
	// silently held somebody's annotations without ever showing them would be
	// keeping them rather than preserving them.
	if highlights, err := s.store.HighlightsForArticle(r.Context(), userID, view.Article.ID); err != nil {
		s.log.Warn("listing highlights failed", "article_id", view.Article.ID, "error", err)
	} else {
		page.Highlights = highlights
	}

	// The other stored copies of this page, when there are any. Looked up on every
	// article read, which is one indexed query against a table that holds one row
	// for most articles — cheaper than the alternative of hiding the control behind
	// a second page nobody would find.
	if bodies, err := s.store.BodiesForArticle(r.Context(), view.Article.ID); err != nil {
		// A failed lookup costs the choice, not the article.
		s.log.Warn("listing the stored bodies failed", "article_id", view.Article.ID, "error", err)
	} else if len(bodies) > 1 {
		for _, b := range bodies {
			page.Bodies = append(page.Bodies, bodyChoice{
				ID:          b.ID,
				Description: describeBody(b),
				WordCount:   b.WordCount,
				ExtractedAt: b.ExtractedAt,
				Excerpt:     b.Excerpt,
				Current:     b.Current,
				Immutable:   b.Immutable,
			})
		}
	}

	page.ImageNotice = imageNoticeFor(view)

	if view.HasBody {
		// Marked safe because extraction sanitized it with bluemonday before
		// it was ever stored, and the CSP on this response blocks script and
		// third-party requests regardless. Neither alone would be enough; together
		// they are why this is not reckless.
		page.Body = template.HTML(view.Content.HTML) //nolint:gosec // sanitized at extraction; see the extraction ladder
	} else {
		page.Notice = noticeFor(view)
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

// imageNoticeFor explains images that are not going to appear.
//
// Worth saying out loud because the failure is invisible and looks like a bug.
// Between extraction and localization an article's markup still points at the
// origin site, and the archive's content security policy blocks every remote
// image — so the reader gets correctly-sized blank rectangles with no
// explanation, for as long as the asset queue takes to reach this article. On a
// fresh import that is hours.
func imageNoticeFor(v store.ArticleView) string {
	if !v.HasBody {
		return ""
	}
	switch v.Article.AssetsStatus {
	case "pending":
		return "The images in this article have not been archived yet, so they are not " +
			"shown. The worker is working through them; reload in a while."
	case "partial":
		return "Some images in this article could not be archived, and are not shown. " +
			"Images are never loaded from the original site."
	default:
		return ""
	}
}

// noticeFor explains an article with no body, in the reader's terms.
func noticeFor(v store.ArticleView) string {
	a := v.Article
	switch {
	case v.ExpiredAt != nil:
		return "The stored copy of this page was released on " +
			v.ExpiredAt.Format("2 January 2006") + " under the retention policy, because it had " +
			"been read and was not starred, kept, or saved. The original is still linked above, " +
			"and re-fetching it will archive it again."
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

	// From is the token result links carry, so that "back" from an article returns
	// to these results rather than to the empty search form. Results are ranked
	// rather than ordered, so it grants no previous/next — see streamSpec.Ordered.
	From string
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("q"))

	page := searchPage{
		pageData: s.pageData(r, "search"),
		Query:    text, Ran: text != "",
		From: searchStream(text),
	}

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

	// Feeds are the rows to draw, which is every subscription the filter did not
	// exclude, in the order View asks for.
	Feeds  []feedRow
	Tags   []store.Tag
	Broken int

	// Total is how many subscriptions there are, so a filtered list can say what it
	// is a subset of. Counted before filtering, like Broken: "6 feeds are failing"
	// is a fact about the archive, not about the search somebody typed.
	Total int

	// View is the ordering and filtering the reader asked for, and the source of
	// every link on the page that has to preserve it.
	View feedView

	// Categories are the folder names already in use, offered as suggestions when
	// filing a new subscription. A category exists only because some feed claims
	// it, so this is the whole list there is.
	Categories []string

	// TestingAvailable says whether this instance can test a feed URL before
	// subscribing, which needs an outbound HTTP client.
	TestingAvailable bool

	// PollChoices are the cadences the edit form offers for one feed, and PollFloor
	// is the instance's shortest, named so the form can say what a shorter choice
	// would be raised to.
	PollChoices []store.PollChoice
	PollFloor   string

	// UsualCadence is the reader's general preference in words, which is what
	// "automatic" on one feed defers to.
	UsualCadence string

	feedsExtras
}

// Editing is the feed loaded into the form, zero when the form is a blank one
// waiting for a new subscription.
//
// A method rather than a field so the template can ask once, without walking into
// a nil Add on every page that had nothing to report.
func (p feedsPage) Editing() store.FeedID {
	if p.Add == nil {
		return 0
	}
	return p.Add.EditingID
}

// pollEvery is the cadence the form holds, which is what the picker has to be built
// around: empty on a page with no form open, and on the add form, which has no
// picker.
func (p feedsPage) pollEvery() string {
	if p.Add == nil {
		return ""
	}
	return p.Add.PollEvery
}

// PollEvery is the same value for the template, so the picker's selected option
// does not have to walk into a nil Add.
func (p feedsPage) PollEvery() string { return p.pollEvery() }

// FormAction is where the one-subscription form posts: the add route, or the edit
// route for the feed it has open.
//
// The view rides along in the query string because these routes render the list
// rather than redirecting to it — see renderFeedsWith — and a POST carries no query
// string of its own, so without this a save would drop the reader back into an
// unsorted, unfiltered list.
func (p feedsPage) FormAction() string {
	if id := p.Editing(); id != 0 {
		return p.View.href("/feeds/" + strconv.FormatInt(int64(id), 10) + "/edit")
	}
	return p.View.href("/feeds/add")
}

// TestAction is the same for the test button, which saves nothing and comes back
// to the same form.
func (p feedsPage) TestAction() string { return p.View.href("/feeds/test") }

// AskingToUnsubscribe reports whether the page is holding a removal question, which
// is when the subscription form stands down: one destructive question, on its own.
func (p feedsPage) AskingToUnsubscribe() bool {
	return p.Unsubscribe != nil && p.Unsubscribe.Confirming
}

// UnsubscribeAction is where the confirmation for a removal posts, with the view
// attached for the same reason the form's is.
func (p feedsPage) UnsubscribeAction() string {
	if p.Unsubscribe == nil {
		return ""
	}
	return p.View.href("/feeds/" +
		strconv.FormatInt(int64(p.Unsubscribe.Feed.ID), 10) + "/unsubscribe")
}

// feedsExtras are the results of whatever the reader just did, if anything.
//
// One struct rather than a parameter each, because every one of them is optional,
// they are mutually exclusive in practice, and a render function with four nil
// pointers in its signature is a call nobody can read.
type feedsExtras struct {
	// Imported is set only on the page rendered straight after an upload.
	Imported *importOutcome

	// Refreshed is set only on the page rendered straight after a manual refresh.
	Refreshed *refreshOutcome

	// Add is set after testing or adding a single subscription.
	Add *addFeedOutcome

	// Unsubscribe is the state of removing one subscription: the question, or what
	// answering it did.
	Unsubscribe *unsubscribeControl
}

// unsubscribeControl is the two steps of removing a subscription.
//
// Two steps for the same reason marking a list read is: it cannot be undone one
// button at a time. Unlike that control, this one has to say what it would cost
// before it is pressed — which is what Removal carries, and why the ask exists as a
// state of the page rather than as a JavaScript dialog the content security policy
// would refuse to run anyway.
type unsubscribeControl struct {
	// Feed is the subscription in question, read before the delete so the
	// confirmation can name it and the result can still name it afterwards.
	Feed store.Feed

	// Removal is what going ahead would cost, and is meaningless once Done.
	Removal store.FeedRemoval

	// Confirming is set on the page that asks; Done on the page that reports.
	Confirming bool
	Done       bool

	// Problem is a reason it did not happen.
	Problem string
}

type feedRow struct {
	store.Feed
	Unread int64

	// CategoryPath links a feed's category to that category's stream, so the feed
	// list is a way into the categories rather than just a report of them.
	CategoryPath string
}

func (s *Server) handleFeeds(w http.ResponseWriter, r *http.Request) {
	extras := feedsExtras{}

	// `?edit=<id>` loads one subscription into the form at the top of the page. A
	// GET parameter rather than a route of its own, because the thing being asked
	// for is this page with the form filled in — the list underneath, the reader's
	// ordering and their filter all still apply.
	if raw := r.URL.Query().Get("edit"); raw != "" {
		extras.Add = s.editForm(r, raw)
	}

	// `?unsubscribe=<id>` asks before removing one. Same reasoning, and it takes the
	// form's place on the page rather than appearing beside it: a destructive question
	// under two save buttons is a question somebody answers by accident.
	if raw := r.URL.Query().Get("unsubscribe"); raw != "" {
		extras.Unsubscribe = s.unsubscribeAsk(r, raw)
	}

	s.renderFeedsWith(w, r, http.StatusOK, extras)
}

// renderFeedsWith draws the feed list, reporting on whatever the reader just did.
//
// Shared by every route that changes subscriptions so that the page after an
// action is the same page, freshly counted — an import that subscribed to seventy
// feeds should show seventy feeds, not a summary line above a stale list.
func (s *Server) renderFeedsWith(w http.ResponseWriter, r *http.Request, status int,
	extras feedsExtras,
) {
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

	page := feedsPage{
		pageData:         s.pageData(r, "feeds"),
		Tags:             tags,
		TestingAvailable: s.fetch != nil,
		feedsExtras:      extras,
		// Read from the request even on a POST: the forms put the reader's ordering
		// and filter in their action URLs precisely so that the page rendered after
		// a save is the page they were on.
		View: feedViewFrom(r.URL.Query()),
	}
	page.Unread = counts.Total
	page.PollFloor = s.pollFloorLabel()
	// The picker is drawn from what the form currently holds, so an interval this
	// release no longer offers is still on the list while that feed is open.
	page.PollChoices = s.pollChoices(page.pollEvery())
	if page.DefaultPollInterval != nil {
		choice, _ := store.PollChoiceFor(page.DefaultPollInterval)
		page.UsualCadence = choice.Phrase
	}

	// The categories in use, for the suggestion list on the add form. Names only:
	// this page never showed the counts, and they are the half of ListCategories that
	// costs an archive-sized aggregate. A failed lookup costs the suggestions, not
	// the page.
	if categories, err := s.store.ListCategoryNames(r.Context(), userID); err != nil {
		s.log.Warn("listing categories for the feed form failed", "error", err)
	} else {
		for _, name := range categories {
			// The nameless category is not a suggestion: filing a feed under it is
			// what leaving the box empty already does.
			if name != "" {
				page.Categories = append(page.Categories, name)
			}
		}
	}

	page.Total = len(feeds)
	for _, f := range feeds {
		if f.ConsecutiveFailures > 0 || f.Disabled {
			page.Broken++
		}
		row := feedRow{
			Feed:         f,
			Unread:       counts.ByFeed[f.ID],
			CategoryPath: categoryPath(f.Category),
		}
		if page.View.matches(row) {
			page.Feeds = append(page.Feeds, row)
		}
	}
	page.View.sortRows(page.Feeds)

	s.render(w, status, "feeds", page)
}

// categoriesPage is the category index: the folders an OPML import produced,
// which is how a reader looks up "Comics" without remembering which feeds are in
// it.
type categoriesPage struct {
	pageData

	Categories []categoryRow

	// Editing and Deleting are the row a form is open on, nil otherwise. Pointers
	// rather than ids so the template needs no second lookup to name what it is
	// about to change.
	Editing  *categoryRow
	Deleting *categoryRow

	// Movable are the other categories a deleted one's feeds could go to, which is
	// every managed row except the one being deleted. Empty when there is nowhere to
	// move them, in which case the form does not offer it — an option leading to an
	// empty picker is worse than no option.
	Movable []categoryRow

	// Saved reports what just happened, Problem why it did not.
	Saved   string
	Problem string
}

type categoryRow struct {
	store.Category

	// Heading is the display name, which differs from Name only for the category
	// that has none.
	Heading string
	Path    string

	// Managed says this row is a real category the reader may rename or delete.
	// False for the nameless bucket, which is the absence of a category rather than
	// one named for absence: there is nothing there to rename, and nothing to
	// delete. See migration 00013.
	Managed bool
}

// handleCategories is both the index and one category's stream.
//
// One route, split on whether `name` is present — the same shape as /search,
// which is a form until it has something to search for. Present-but-empty selects
// the feeds with no category, which is why this tests for the parameter rather
// than for a non-empty value.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("name") {
		s.serveStream(w, r, s.categorySpec(r.URL.Query().Get("name")))
		return
	}

	s.renderCategories(w, r, http.StatusOK, "", "")
}

// renderCategories draws the index, optionally reporting on what just happened.
//
// Rendered rather than redirected, like every other form here: the outcome belongs
// to the request that earned it, and a redirect would lose the message.
func (s *Server) renderCategories(w http.ResponseWriter, r *http.Request, status int, saved, problem string) {

	categories, err := s.store.ListCategories(r.Context(), signedInUser(r))
	if err != nil {
		s.log.Error("listing categories failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := categoriesPage{pageData: s.pageData(r, "categories")}
	for _, c := range categories {
		page.Categories = append(page.Categories, categoryRow{
			Category: c,
			Heading:  categoryHeading(c.Name),
			Path:     categoryPath(c.Name),
			Managed:  c.Name != "",
		})
	}

	// Asking about a deletion, which is a page rather than a dialog for the reason
	// the bulk mark's is: the content security policy has no 'unsafe-inline', and a
	// question that is a URL is reloadable, linkable, and canceled by navigating
	// away.
	if raw := r.URL.Query().Get("delete"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			for _, row := range page.Categories {
				if row.Managed && int64(row.ID) == id {
					page.Deleting = &row
				}
			}
			if page.Deleting == nil {
				// A category they do not have. Not an error page: the list they asked
				// for is right here, and the question simply has no subject.
				s.log.Warn("asked to delete a category that is not there",
					"category_id", id, "user_id", signedInUser(r))
			}
		}
	}
	if raw := r.URL.Query().Get("edit"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			for _, row := range page.Categories {
				if row.Managed && int64(row.ID) == id {
					page.Editing = &row
				}
			}
		}
	}
	if page.Deleting != nil {
		for _, row := range page.Categories {
			if row.Managed && row.ID != page.Deleting.ID {
				page.Movable = append(page.Movable, row)
			}
		}
	}
	page.Saved, page.Problem = saved, problem

	s.render(w, status, "categories", page)
}

// attentionPage is the failed-fetch queue.
type attentionPage struct {
	pageData
	Items []attentionRow
}

// attentionRow is one entry, with the way out of it.
type attentionRow struct {
	store.NeedsAttention

	// Host is the article's host, and RulePath is the form for writing a rule
	// against it. This page is where a badly-extracted site is discovered, and
	// until now the fix started by leaving the browser — which is most of why
	// rules got written rarely.
	Host     string
	RulePath string
}

func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.NeedsAttentionFor(r.Context(), signedInUser(r), store.MaxStreamLimit)
	if err != nil {
		s.log.Error("listing articles needing attention failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := attentionPage{pageData: s.pageData(r, "attention")}
	for _, item := range items {
		row := attentionRow{NeedsAttention: item}
		if host, ok := hostOf(item.URLCanonical); ok {
			row.Host = host
			row.RulePath = "/domain-rules?edit=" + url.QueryEscape(host)
		}
		page.Items = append(page.Items, row)
	}

	s.render(w, http.StatusOK, "attention", page)
}

// actions is the data behind the read/star controls, rendered both inside a page
// and alone as an htmx response.
type actions struct {
	ArticleID store.ArticleID
	Read      bool
	Starred   bool
	Kept      bool

	// OOB asks for an out-of-band swap, for a response that carries controls it was
	// not aimed at. See the comment on the partial.
	OOB bool
}

func (s *Server) handleToggleRead(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, s.store.SetRead)
}

func (s *Server) handleToggleStar(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, s.store.SetStarred)
}

func (s *Server) handleToggleKept(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, s.store.SetKept)
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

	// Every field, including the one this request did not change. The fragment
	// replaces all three controls, so a missing Kept here rendered the keep button
	// as "not kept" after any star or read toggle — the state was still in the
	// database, but the reader was told otherwise until they reloaded.
	s.renderFragment(w, http.StatusOK, "actions", actions{
		ArticleID: view.Article.ID,
		Read:      view.Read,
		Starred:   view.Starred,
		Kept:      view.Kept,
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

// pageURL adds a parameter to a path that may already carry one.
//
// A category's path does — it identifies the category by query parameter — so
// appending "?before=..." unconditionally would produce a URL with two question
// marks and a next-page link that silently paged from the beginning.
func pageURL(path, key, value string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + url.QueryEscape(key) + "=" + url.QueryEscape(value)
}

// selfPath is this request's own path and query, for the reload control.
//
// A path beginning with two slashes is rejected: `//example.com/` in an href is
// protocol-relative, so echoing the request line back into a link would turn a
// reload button into an off-site one. ServeMux cleans such paths before a handler
// sees them, which makes this belt as well as braces — and belt as well as braces
// is the right amount for the one place a request line reaches an href.
func selfPath(r *http.Request) string {
	uri := r.URL.RequestURI()
	if !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return "/"
	}
	return uri
}

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
