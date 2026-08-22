package store

// This file holds the one definition of "which body a reader sees", in the same
// spirit as visibleArticles in reading.go: one predicate, embedded everywhere,
// never a second copy inlined at a call site.
//
// The tenancy line it implements:
//
//   The household owns what costs bandwidth or disk — the fetched page and the
//   content-addressed images. One poll, one raw copy, one image however many
//   readers hold it.
//
//   The reader owns what is derived from it — the body, the rules that produced
//   it, and which stored copy they see.
//
// `article_content.user_id` is NULL for the household's extraction, which is what
// every reader gets until their own extraction diverges. A reader who never writes
// a domain rule never has a row of their own, so the common case costs nothing.

// preferredBody chooses between the two rows a reader may have for one article:
// their own extraction wins, and the household's serves when they have none.
//
// Written as a filter rather than as "order by owner, limit 1", which is what this
// was first. The ordering form reads more directly and cannot be used by search:
// picking the row in a lateral subquery makes `articles` the driving table, so the
// GIN index on article_content.tsv goes unused and every query scans the archive.
// As a filter, article_content stays a real join relation and the planner may still
// start from the index.
//
// The NOT EXISTS is what keeps it to one row. Without it a reader with a fork
// matches both their row and the household's, and every list would show that
// article twice.
//
// **Every query embedding this must pass the reader's id as $1**, the same
// coupling visibleArticles carries and for the same reason: one definition that is
// obviously wrong when misused beats several that are subtly right.
const preferredBody = `(
	c.user_id = $1
	OR (c.user_id IS NULL AND NOT EXISTS (
		SELECT 1 FROM article_content mine
		 WHERE mine.article_id = a.id AND mine.is_current AND mine.user_id = $1
	))
)`

// ownedBody joins the body a reader sees, aliased `c`, to an articles row aliased
// `a`.
//
// LEFT JOIN, not JOIN: an article with no body at all is still an article, and the
// attention queue exists precisely to list those. A plain join would make them
// vanish from every list that embeds this, which is the silent-exclusion shape this
// project has now been bitten by four times.
const ownedBody = `
	LEFT JOIN article_content c
	    ON c.article_id = a.id AND c.is_current AND ` + preferredBody

// ownedBodyInner is the same join for queries where an article without a body is
// not a result at all — search being the case: there is nothing to match against
// and nothing to show.
const ownedBodyInner = `
	JOIN article_content c
	    ON c.article_id = a.id AND c.is_current AND ` + preferredBody

// ownedBodyExists is the same rule as a predicate, for queries that only need to
// know whether this reader has a body rather than what is in it.
//
// Kept beside ownedBody so the two cannot drift: a list that says "has a body" by
// one rule and renders a body by another would disagree with itself on exactly the
// articles a reader would ask about.
const ownedBodyExists = `EXISTS (
	SELECT 1 FROM article_content c
	 WHERE c.article_id = a.id AND c.is_current
	   AND (c.user_id = $1 OR c.user_id IS NULL)
)`

// Household names the body slot every reader reads until their own extraction
// diverges — the NULL owner.
//
// A function returning nil rather than a bare nil at each call site, because
// `CurrentContent(ctx, id, nil)` does not say which of the two possible meanings
// of nil it intends, and this is the one place where guessing wrong overwrites
// what everybody reads. It also cannot be reassigned, which a package-level
// variable could.
func Household() *UserID { return nil }

// Owned names one reader's slot.
func Owned(id UserID) *UserID { return &id }
