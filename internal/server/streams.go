package server

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A reader is always looking at *a list*. Which one they came from is what makes
// "back", "previous" and "next" answerable on an article page, and the browser
// cannot answer it: installed as a web app there is no back button, and even in a
// browser the back button cannot say what the *next* article is.
//
// So the list travels with the link, as `?from=<token>`. The token is resolved
// back into a streamSpec on the article page, which is why there is one table of
// list definitions rather than one per handler — a Next button computed from a
// second, drifted definition of "the unread list" would skip articles the list
// had shown, and nothing would catch it but a reader noticing.
const (
	streamUnread    = "unread"
	streamAll       = "all"
	streamStarred   = "starred"
	streamSaved     = "saved"
	streamAttention = "attention"

	// These carry an argument after the colon.
	streamFeed     = "feed:"
	streamTag      = "tag:"
	streamCategory = "category:"
	streamSearch   = "search:"

	// The unread list narrowed to one category — two dimensions in one token.
	//
	// The variable part stays last, which is the constraint the whole grammar rests
	// on: a category is a folder name from somebody else's reader and may contain a
	// colon, so anything after the prefix has to be taken whole rather than split.
	// `unread:category:X` would be unparseable for a category named "Cooking: BBQ".
	streamUnreadCategory = "unread-category:"
)

// streamSpec is one list: what to call it, where it lives, and what it contains.
type streamSpec struct {
	// Token is what travels in `?from=`. Empty means "no list", which is what an
	// article reached by a bare link gets.
	Token string

	// Nav is the section the chrome highlights.
	Nav string

	// Heading titles the list, and doubles as the label on the control that goes
	// back to it.
	Heading string

	// Empty is what to say when the list has nothing in it.
	Empty string

	// Path is where the list lives, ready to put in an href.
	Path string

	// Query selects the list's contents. Meaningless when Ordered is false.
	Query store.StreamQuery

	// Ordered says whether this list has a well-defined article order, and so
	// whether previous and next mean anything in it.
	//
	// False for search, which is ranked by relevance against a query string the
	// store would have to re-run, and for the attention queue, which is a
	// worklist rather than a reading order.
	Ordered bool

	// Narrowable says whether this list offers the category control.
	//
	// Opt-in, like Markable, because for most lists narrowing by category is either
	// meaningless or a worse version of something they already are: one feed is
	// already one category's member, a tag deliberately crosses categories, search
	// has its own query, and the reading list holds pages that came from no feed at
	// all and so have no category to filter by. It belongs on the two lists that
	// span every subscription — unread and everything — and on the category views
	// themselves, where it is how a reader moves sideways.
	Narrowable bool

	// Markable says whether this list may be marked read in bulk.
	//
	// Opt-in, and the reason is not taste. Marking a list read applies Query to
	// every article it selects, so a list whose Query does not describe its
	// contents would mark something else entirely: search and the attention queue
	// both carry an empty Query — they are built from a query string and a status
	// filter respectively — and offering them this control would mark the reader's
	// whole archive read from a page showing four results. A new stream is
	// therefore unmarkable until somebody says otherwise.
	//
	// Left off the hand-picked lists too, for a plainer reason: starring and saving
	// are per-article decisions, so a bulk control over them answers a question
	// nobody asked.
	Markable bool
}

// feedStream and friends build the token for a list that needs an argument.
func feedStream(id store.FeedID) string { return streamFeed + strconv.FormatInt(int64(id), 10) }
func tagStream(id store.TagID) string   { return streamTag + strconv.FormatInt(int64(id), 10) }

// categoryStream builds the token for a category, including the one with no name.
//
// The empty category is a real bucket — feeds an OPML file listed outside any
// folder — so it gets a token like any other, and the emptiness is carried in the
// argument rather than by leaving the token off.
func categoryStream(name string) string { return streamCategory + name }

func searchStream(text string) string { return streamSearch + text }

// categoryPill is one entry in the control that narrows a list by category.
type categoryPill struct {
	Label string
	Href  string

	// Current marks the entry the reader is looking at, which the template turns
	// into aria-current as well as a style — the row is navigation, and "you are
	// here" has to be available to somebody who cannot see the styling.
	Current bool
}

// categoryPills builds that control for one list, or nil when the list does not take
// one or there is nothing to choose between.
//
// Where each entry points depends on which list is asking, and that is deliberate.
// Narrowing the unread list yields the unread list narrowed, which exists nowhere
// else. Narrowing everything yields the category's own view — which already exists,
// under Categories, and is the canonical address for those articles. Linking there
// instead of inventing a second address for the same list is what keeps the way back
// from an article honest: the list a reader is standing on and the list the article
// remembers being opened from stay the same list.
func (s *Server) categoryPills(ctx context.Context, userID store.UserID, spec streamSpec) []categoryPill {
	// Checked before the query as well as inside categoryPillsFor, so a list that
	// takes no control costs no round trip to find that out.
	if !spec.Narrowable {
		return nil
	}

	names, err := s.store.ListCategoryNames(ctx, userID)
	if err != nil {
		// A missing control costs a shortcut, not the list the reader asked for —
		// the same trade the unread tally and the mark-read count make.
		s.log.Warn("listing category names failed", "from", spec.Token, "error", err)
		return nil
	}
	return categoryPillsFor(spec, names)
}

// categoryPillsFor is the control's contents for one list and the categories in use.
//
// Split from the query above so that the part with the decisions in it — where each
// entry points, which one is current, whether the control is worth drawing at all —
// is exercised directly rather than through a database and a rendered page.
func categoryPillsFor(spec streamSpec, names []string) []categoryPill {
	if !spec.Narrowable {
		return nil
	}
	// One bucket is not a choice, and an archive whose feeds are all unfiled has
	// exactly one. Drawing a control that can only select what is already selected
	// would be furniture.
	if len(names) < 2 {
		return nil
	}

	// Unread lists narrow onto themselves; everything else narrows onto the category
	// views, which is where an unfiltered category lives.
	unread := spec.Query.UnreadOnly

	pills := make([]categoryPill, 0, len(names)+1)

	// The way out comes first, because "show me everything again" is the destination
	// somebody needs to find without reading the row.
	out := categoryPill{Label: "All categories", Href: "/all", Current: !spec.Query.Categorized}
	if unread {
		out.Href = "/"
	}
	pills = append(pills, out)

	for _, name := range names {
		href := categoryPath(name)
		if unread {
			href = "/?" + url.Values{"category": {name}}.Encode()
		}
		pills = append(pills, categoryPill{
			Label:   categoryHeading(name),
			Href:    href,
			Current: spec.Query.Categorized && spec.Query.Category == name,
		})
	}
	return pills
}

// UncategorizedHeading is what the category with no name is called.
//
// Only ever displayed. Naming it in the data would mean a real category called
// "Uncategorized" — which the sample OPML in fact contains — silently merging
// with the nameless one.
const UncategorizedHeading = "No category"

// categoryHeading names a category for display.
func categoryHeading(name string) string {
	if name == "" {
		return UncategorizedHeading
	}
	return name
}

// categoryPath is where one category's stream lives.
//
// A query parameter rather than a path segment, because a category is a folder
// name from somebody else's reader: it can contain a slash (nested folders are
// joined that way on import), spaces, or anything else a person typed. Escaped
// into a segment that becomes %2F, which is the kind of thing that works until it
// meets a proxy. As a parameter it is just a string.
func categoryPath(name string) string {
	return "/categories?" + url.Values{"name": {name}}.Encode()
}

// streamSpecFor resolves a token to the list it names.
//
// Reports false for a token it does not recognize, or one naming a feed this
// reader does not have. Callers treat that as "no list": the article still
// renders, without the controls that would have needed one. That is the right
// failure for a hand-edited URL, and it is also what a reader gets from a link
// somebody sent them.
func (s *Server) streamSpecFor(ctx context.Context, userID store.UserID, token string) (streamSpec, bool) {
	switch token {
	case streamUnread:
		return s.unreadSpec(), true
	case streamAll:
		return s.allSpec(), true
	case streamStarred:
		return s.starredSpec(), true
	case streamSaved:
		return s.savedSpec(), true
	case streamAttention:
		return streamSpec{
			Token: streamAttention, Nav: "attention",
			Heading: "Attention", Path: "/attention",
		}, true
	}

	arg := func(prefix string) string { return strings.TrimPrefix(token, prefix) }

	switch {
	case strings.HasPrefix(token, streamFeed):
		id, err := strconv.ParseInt(arg(streamFeed), 10, 64)
		if err != nil || id <= 0 {
			return streamSpec{}, false
		}
		feed, err := s.store.GetFeed(ctx, userID, store.FeedID(id))
		if err != nil {
			// Includes "not one of yours", which GetFeed reports as not found for
			// the same reason article lookups do.
			return streamSpec{}, false
		}
		return s.feedSpec(feed), true

	case strings.HasPrefix(token, streamTag):
		id, err := strconv.ParseInt(arg(streamTag), 10, 64)
		if err != nil || id <= 0 {
			return streamSpec{}, false
		}
		return s.tagSpec(store.TagID(id)), true

	case strings.HasPrefix(token, streamUnreadCategory):
		// Tested before the category prefix only for readability; the two cannot
		// collide, because "unread-category:" does not begin with "category:".
		return s.unreadCategorySpec(arg(streamUnreadCategory)), true

	case strings.HasPrefix(token, streamCategory):
		// Not validated against the reader's categories, deliberately. A category
		// is only ever a filter over their own feeds, so an unknown one yields an
		// empty list — which is honest, and tells an inquisitive visitor nothing
		// they did not already supply.
		return s.categorySpec(arg(streamCategory)), true

	case strings.HasPrefix(token, streamSearch):
		text := arg(streamSearch)
		return streamSpec{
			Token: token, Nav: "search",
			Heading: "Search",
			Path:    "/search?" + url.Values{"q": {text}}.Encode(),
		}, true
	}

	return streamSpec{}, false
}

func (s *Server) unreadSpec() streamSpec {
	return streamSpec{
		Token: streamUnread, Nav: "unread", Heading: "Unread", Path: "/",
		Empty:      "Nothing unread. The worker fills this in as feeds are polled.",
		Query:      store.StreamQuery{UnreadOnly: true},
		Ordered:    true,
		Narrowable: true,
		Markable:   true,
	}
}

// unreadCategorySpec is the unread list narrowed to one category.
//
// The one combination that had no home. A category's own view is everything it has
// ever carried, newest first, which is the right thing for going back to a folder —
// but "what is new in the comics, and nothing else" was reachable only by reading
// past everything else in Unread. Because it is a spec like any other, it inherits
// paging, prev/next, and a mark-read control scoped to exactly these articles, which
// is the useful half: clearing one folder without touching the rest.
//
// It lives on the unread path rather than at a URL of its own, because that is what
// it is — the unread list, narrowed. The category's unfiltered archive keeps its own
// address under Categories.
func (s *Server) unreadCategorySpec(name string) streamSpec {
	return streamSpec{
		Token: streamUnreadCategory + name, Nav: "unread",
		Heading: unreadCategoryHeading(name),
		Path:    "/?" + url.Values{"category": {name}}.Encode(),
		Empty: "Nothing unread from the feeds in this category. Everything they have " +
			"carried is still in the archive, under Categories.",
		Query:      store.StreamQuery{UnreadOnly: true, Category: name, Categorized: true},
		Ordered:    true,
		Narrowable: true,
		Markable:   true,
	}
}

// unreadCategoryHeading titles the narrowed unread list.
//
// It also becomes the label on the article page's way back, so it has to read as a
// place rather than as a description: "Back to Unread in Comics" works, and the
// nameless category gets its own phrasing because "Unread in No category" does not.
func unreadCategoryHeading(name string) string {
	if name == "" {
		return "Unread with no category"
	}
	return "Unread in " + name
}

func (s *Server) allSpec() streamSpec {
	return streamSpec{
		Token: streamAll, Nav: "all", Heading: "Everything", Path: "/all",
		Empty:      "The archive is empty. Import feeds and run the worker.",
		Ordered:    true,
		Narrowable: true,
		Markable:   true,
	}
}

func (s *Server) starredSpec() streamSpec {
	return streamSpec{
		Token: streamStarred, Nav: "starred", Heading: "Starred", Path: "/starred",
		Empty:   "Nothing starred yet. Press s on an article, or use the button in the reader.",
		Query:   store.StreamQuery{StarredOnly: true},
		Ordered: true,
	}
}

func (s *Server) savedSpec() streamSpec {
	return streamSpec{
		Token: streamSaved, Nav: "saved", Heading: "Saved", Path: "/saved",
		Empty:   "Nothing saved yet. Paste a link above to archive a page.",
		Query:   store.StreamQuery{SavedOnly: true},
		Ordered: true,
	}
}

func (s *Server) feedSpec(feed store.Feed) streamSpec {
	return streamSpec{
		Token: feedStream(feed.ID), Nav: "feeds", Heading: feed.Title,
		Path:     "/feeds/" + strconv.FormatInt(int64(feed.ID), 10),
		Empty:    "Nothing stored from this feed yet.",
		Query:    store.StreamQuery{FeedID: feed.ID},
		Ordered:  true,
		Markable: true,
	}
}

func (s *Server) tagSpec(id store.TagID) streamSpec {
	return streamSpec{
		Token: tagStream(id), Nav: "tags", Heading: "Tagged",
		Path:     "/tags/" + strconv.FormatInt(int64(id), 10),
		Empty:    "Nothing carries this tag.",
		Query:    store.StreamQuery{TagID: id},
		Ordered:  true,
		Markable: true,
	}
}

// categorySpec is one category's whole archive, newest first.
//
// Deliberately not unread-only, which is where this differs from most readers —
// Miniflux and FreshRSS both default a folder to its unread items. A category here is
// the way back to a folder: "show me the comics" means the comics, and the order
// already puts anything new at the top, so unread items are where a reader looks
// first without the list hiding what it has. The unread-only version of the same
// scope is its own list; see unreadCategorySpec.
func (s *Server) categorySpec(name string) streamSpec {
	return streamSpec{
		Token: categoryStream(name), Nav: "categories",
		Heading:    categoryHeading(name),
		Path:       categoryPath(name),
		Empty:      "Nothing stored from the feeds in this category yet.",
		Query:      store.StreamQuery{Category: name, Categorized: true},
		Ordered:    true,
		Narrowable: true,
		Markable:   true,
	}
}
