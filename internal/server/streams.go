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
		Empty:   "Nothing unread. The worker fills this in as feeds are polled.",
		Query:   store.StreamQuery{UnreadOnly: true},
		Ordered: true,
	}
}

func (s *Server) allSpec() streamSpec {
	return streamSpec{
		Token: streamAll, Nav: "all", Heading: "Everything", Path: "/all",
		Empty:   "The archive is empty. Import feeds and run the worker.",
		Ordered: true,
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
		Path:    "/feeds/" + strconv.FormatInt(int64(feed.ID), 10),
		Empty:   "Nothing stored from this feed yet.",
		Query:   store.StreamQuery{FeedID: feed.ID},
		Ordered: true,
	}
}

func (s *Server) tagSpec(id store.TagID) streamSpec {
	return streamSpec{
		Token: tagStream(id), Nav: "tags", Heading: "Tagged",
		Path:    "/tags/" + strconv.FormatInt(int64(id), 10),
		Empty:   "Nothing carries this tag.",
		Query:   store.StreamQuery{TagID: id},
		Ordered: true,
	}
}

func (s *Server) categorySpec(name string) streamSpec {
	return streamSpec{
		Token: categoryStream(name), Nav: "categories",
		Heading: categoryHeading(name),
		Path:    categoryPath(name),
		Empty:   "Nothing stored from the feeds in this category yet.",
		Query:   store.StreamQuery{Category: name, Categorized: true},
		Ordered: true,
	}
}
