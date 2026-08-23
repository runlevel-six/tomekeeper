package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// The audit queries: three ways of asking whether a stored body is what it claims
// to be.
//
// They exist because of a body that read perfectly well and was not the article. A
// re-fetch of two stackoverflow.blog posts went through a headless browser, landed on
// the site's cookie consent gate, and readability lifted the consent dialog as the
// article — 410 confident words, `fetch_status = ok`, and gone from the failed-fetch
// queue. Nothing in the ladder could have known: the thresholds it enforces are
// length and share-of-page, and a consent dialog passes both comfortably.
//
// **None of these is a gate, and that is deliberate.** Checked item by item against a
// real archive of 2,211 bodies, the title lens flags seven and **not one of them is a
// body extraction got wrong**: two are artifacts of the URL-title bug, and the rest are
// two podcast episode pages, a link roundup, a digest in Russian, and a store homepage
// with no article in it to find. As a rejection rung it would have discarded a
// 16,249-word body and five legitimate ones to catch nothing.
//
// That is not a case against the lens — the failure it exists for is real and cost a
// day — but it is decisive about what the lens may do. It reports, and a person decides.
//
// (An earlier estimate here said "about three are real". That was inferred from word
// counts and domains rather than read, and reading them refuted it. A 111-word body on
// a domain known to serve shells turned out to be a podcast episode description.)
//
// Read-only. Nothing here writes.

// auditScope narrows a lens to what one reader may see.
//
// Each lens exists in two forms. `tome audit` asks an operator's question — is
// anything in this archive wrong — and looks at every stored body. The audit page
// asks a reader's — does anything *I* read look wrong — and may look only at their
// articles, and only at the body each of those shows them.
//
// The two differ at three edges and nowhere else: which articles are in scope,
// which of an article's bodies is the one being judged, and which parameter the
// limit takes once a reader id has claimed $1. So each lens is written once and
// narrowed here. A scoped copy of a lens is not a variant to be maintained
// alongside the original — it is the same lens, and two copies that drifted would
// report different answers about the same archive with nothing to say which was
// right.
type auditScope struct {
	// articles decides which articles the lens may see.
	articles string

	// body decides which of an article's bodies it judges.
	body string

	// limit is the placeholder LIMIT takes.
	limit string
}

// wholeArchive is the operator's scope: every article, every current body,
// deliberately unnarrowed.
//
// Written as a predicate that is always true rather than by omitting the clause,
// so the two forms of every lens are the same string with different edges instead
// of two shapes assembled by conditionals.
var wholeArchive = auditScope{articles: "(true)", body: "(true)", limit: "$1"}

// readerView is one reader's scope: the articles they can see, and the body each
// of those articles shows them.
//
// Both predicates take the reader as $1, which is the coupling documented on each
// of them. preferredBody is the right predicate even where the question is merely
// whether a body exists: it selects the reader's own row where they have one and
// the household's otherwise, so exactly one row matches either way and "is there
// one" gets the same answer as ownedBodyExists would give.
var readerView = auditScope{articles: visibleArticles, body: preferredBody, limit: "$2"}

// SuspectBody is a body with nothing in it that the title talks about.
type SuspectBody struct {
	ArticleID  ArticleID
	Title      string
	URL        string
	Extractor  string
	WordCount  int
	TitleWords int
}

// SuspectBodies returns bodies sharing no distinctive word with their article's title.
//
// The lens that would have caught the consent dialog: a cookie notice has nothing to
// say about "what's left for infrastructure as code", so the overlap is zero.
//
// Three deliberate narrowings, each of which was measured:
//
//   - **Only trafilatura and readability.** They are the rungs that choose a block of
//     a page and can therefore choose the wrong one. A `domain_rule` body is 217 for
//     217 here, because a hand-written selector cannot wander. And `page_images` is
//     15% zero-overlap by design — its bodies are pictures, so a comic legitimately
//     has no prose to match, and flagging those would bury the list in the articles
//     this archive is most careful about.
//
//     This also settles the immutable bodies, which nothing here should propose
//     re-extracting: the one place that marks a body immutable names its extractor
//     `imported` in the same breath, so the rung list already excludes every one of
//     them. A separate `NOT immutable` clause was here until a neuter proved no test
//     could make it matter, which is the only evidence that a guard is load-bearing.
//
//   - **Words over three characters, at least three of them.** A title with fewer
//     distinctive words than that carries no signal to measure, and shorter words
//     ("the", "and", "it") match everything.
//
//   - **Substring, not full-text.** The `tsv` column is tempting and worse: it is
//     built with the `english` configuration, so a Russian title stems to lexemes that
//     cannot match a Russian body, and the same query surfaced nine legitimate podcast
//     pages while losing the one real find. A literal substring is language-agnostic.
//
// Immutable bodies are excluded. The remedy for a suspect body is a domain rule and a
// re-extract, and neither can touch a body that may be the only surviving copy of a
// page that is gone.
func (s *Store) SuspectBodies(ctx context.Context, limit int) ([]SuspectBody, error) {
	return s.suspectBodies(ctx, wholeArchive, limit)
}

// SuspectBodiesFor is the same lens over one reader's articles and their own
// bodies.
//
// A reader may hold their own extraction of a page the household also has, and
// theirs is the one to judge: it is what they read, and it is the one their rule
// produced. Judging the household's copy would report a problem they do not have
// and hide the one they do.
func (s *Store) SuspectBodiesFor(ctx context.Context, userID UserID, limit int) ([]SuspectBody, error) {
	return s.suspectBodies(ctx, readerView, userID, limit)
}

func (s *Store) suspectBodies(ctx context.Context, scope auditScope, args ...any) ([]SuspectBody, error) {
	// Scored per *body* rather than per article, which is what tenancy made
	// necessary. One article can now have two current bodies — the household's and a
	// reader's fork — and grouping by the article mixed them: the title's words were
	// counted twice, `found` became "some body mentions the title" so one good body
	// hid a bad one, and the final join emitted the same finding once per body. All
	// three go away by asking the question the lens was always asking, which is about
	// a body and not about an article.
	rows, err := s.pool.Query(ctx, `
		WITH tok AS (
			SELECT c.id AS body_id,
			       a.id AS article_id,
			       lower(w.word) AS word,
			       lower(c.content_text) AS body
			FROM articles a
			JOIN article_content c ON c.article_id = a.id AND c.is_current
			                      AND `+scope.body+`
			CROSS JOIN LATERAL regexp_split_to_table(
				regexp_replace(a.title, '[^[:alnum:]]+', ' ', 'g'), ' ') AS w(word)
			WHERE c.extractor_name IN ('trafilatura', 'readability')
			  AND length(w.word) > 3
			  AND `+scope.articles+`
		),
		scored AS (
			SELECT body_id, article_id,
			       count(*) AS title_words,
			       count(*) FILTER (WHERE position(word IN body) > 0) AS found
			FROM tok GROUP BY body_id, article_id
		)
		SELECT s.article_id, a.title, a.url_canonical, c.extractor_name, c.word_count, s.title_words
		FROM scored s
		JOIN article_content c ON c.id = s.body_id
		JOIN articles a ON a.id = s.article_id
		WHERE s.found = 0 AND s.title_words >= 3
		ORDER BY c.word_count, s.body_id
		LIMIT `+scope.limit, args...)
	if err != nil {
		return nil, fmt.Errorf("looking for bodies that do not match their title: %w", err)
	}
	defer rows.Close()

	var out []SuspectBody
	for rows.Next() {
		var b SuspectBody
		if err := rows.Scan(&b.ArticleID, &b.Title, &b.URL, &b.Extractor, &b.WordCount, &b.TitleWords); err != nil {
			return nil, fmt.Errorf("scanning a suspect body: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SharedBody is one body text serving more than one article.
type SharedBody struct {
	ArticleIDs []ArticleID
	Hosts      []string
	WordCount  int
	Opening    string
	Immutable  bool
}

// SharedBodies returns bodies that are byte-identical across articles.
//
// The second lens, and the one with almost no false positives: two articles do not
// coincidentally have the same prose. What they do have is the same wall — a consent
// gate, a sign-in page, an "enable JavaScript" notice — extracted as though it were
// content.
//
// It is a weaker *guard* than it looks, and the case that prompted all of this shows
// why: the two consent bodies were only identical to each other because both articles
// happened to be re-fetched in the same batch. One shell post fetched on its own
// produces one consent body and nothing to compare it against. So this catches a
// pattern after it has repeated, which is what an audit is for and not what a gate
// would need.
//
// **Immutable bodies are included here**, unlike the title lens, because excluding them
// loses the only real finding this query has ever made on a live archive: an
// authentication portal — "Sign in with Midway" — stored as the body of two imported
// articles. The finding is worth having even where the remedy is not re-extraction.
func (s *Store) SharedBodies(ctx context.Context, limit int) ([]SharedBody, error) {
	return s.sharedBodies(ctx, wholeArchive, limit)
}

// SharedBodiesFor is the same lens over one reader's articles and their own
// bodies.
//
// A pair only reports when the reader can see both halves of it, which is what
// scoping means here and is a real narrowing rather than an incidental one: an
// identical body on an article they cannot see is exactly the finding they must not
// be shown, because the pairing itself would tell them that article exists.
func (s *Store) SharedBodiesFor(ctx context.Context, userID UserID, limit int) ([]SharedBody, error) {
	return s.sharedBodies(ctx, readerView, userID, limit)
}

func (s *Store) sharedBodies(ctx context.Context, scope auditScope, args ...any) ([]SharedBody, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT array_agg(a.id ORDER BY a.id),
		       array_agg(DISTINCT regexp_replace(
		           substring(a.url_canonical FROM '://([^/]+)'), '^www\.', '')),
		       min(c.word_count),
		       left(regexp_replace(min(c.content_text), '\s+', ' ', 'g'), 90),
		       bool_or(c.immutable)
		FROM article_content c
		JOIN articles a ON a.id = c.article_id
		WHERE c.is_current
		  -- Short bodies collide for dull reasons: a one-line stub, an empty
		  -- paragraph, a "read more" teaser repeated across a feed's items. The
		  -- finding here is boilerplate presented as an article, which is never
		  -- twenty words.
		  AND c.word_count > 20
		  AND `+scope.body+`
		  AND `+scope.articles+`
		GROUP BY md5(c.content_text)
		-- Distinct *articles*, which tenancy made the load-bearing word. Two current
		-- bodies of one article — the household's and a reader's fork — can be
		-- byte-identical when a rule happens to select the same text, and counting
		-- rows would report that article as sharing a body with itself. The finding
		-- here is one wall serving as the article for two different pages.
		HAVING count(DISTINCT a.id) > 1
		ORDER BY count(*) DESC, min(c.word_count)
		LIMIT `+scope.limit, args...)
	if err != nil {
		return nil, fmt.Errorf("looking for bodies shared between articles: %w", err)
	}
	defer rows.Close()

	var out []SharedBody
	for rows.Next() {
		var b SharedBody
		if err := rows.Scan(&b.ArticleIDs, &b.Hosts, &b.WordCount, &b.Opening, &b.Immutable); err != nil {
			return nil, fmt.Errorf("scanning a shared body: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PlaceholderTitle is an article whose title is not a title.
type PlaceholderTitle struct {
	ArticleID ArticleID
	Title     string
	URL       string
	HasBody   bool
}

// PlaceholderTitles returns articles titled with a URL or an encoded filename.
//
// Found while measuring the title lens, as its largest source of noise: two of the
// seven articles it flagged were flagged because their *titles* were broken, not their
// bodies. One was a 16,249-word body under the title
// `eBPF%20and%20the%20Cilium%20Datapath.pdf`.
//
// They arrive from an import whose source had no title for a bookmark, so the URL was
// kept as one, and extraction never replaced it because UpdateArticleMetadata fills
// gaps only and a URL is not a gap. Both halves are fixed — the importer no longer
// writes one, and a placeholder now counts as a gap — so this query is what finds the
// ones already stored. An article with a body gets a real title from the next
// `tome reextract`; a bodyless one has no page to take a title from and needs a fetch
// first, which is worth being able to see separately.
func (s *Store) PlaceholderTitles(ctx context.Context, limit int) ([]PlaceholderTitle, error) {
	return s.placeholderTitles(ctx, wholeArchive, limit)
}

// PlaceholderTitlesFor is the same lens over one reader's articles.
//
// A title is the household's — one row on `articles`, shared by everyone, which is
// why a reader's rule may not rewrite it. So the finding is the same for every
// reader who can see the article; what is scoped is which articles those are, and
// whether *this* reader has a body to take a real title from.
func (s *Store) PlaceholderTitlesFor(ctx context.Context, userID UserID, limit int) ([]PlaceholderTitle, error) {
	return s.placeholderTitles(ctx, readerView, userID, limit)
}

func (s *Store) placeholderTitles(ctx context.Context, scope auditScope, args ...any) ([]PlaceholderTitle, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.title, a.url_canonical,
		       EXISTS(SELECT 1 FROM article_content c
		              WHERE c.article_id = a.id AND c.is_current
		                AND `+scope.body+`)
		FROM articles a
		WHERE `+placeholderTitleSQL+`
		  AND `+scope.articles+`
		ORDER BY a.id
		LIMIT `+scope.limit, args...)
	if err != nil {
		return nil, fmt.Errorf("looking for titles that are URLs: %w", err)
	}
	defer rows.Close()

	var out []PlaceholderTitle
	for rows.Next() {
		var t PlaceholderTitle
		if err := rows.Scan(&t.ArticleID, &t.Title, &t.URL, &t.HasBody); err != nil {
			return nil, fmt.Errorf("scanning a placeholder title: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// placeholderTitleSQL matches a title that is a URL or an encoded filename.
//
// Kept as one string because two places need the same answer and they must not drift:
// this query, which reports them, and UpdateArticleMetadata, which treats one as a gap
// so that extraction may replace it. TitleIsPlaceholder is the same rule in Go, for
// the importer, and is tested against the same table of cases.
//
// Deliberately narrow. A scheme followed by `://` and a percent-escape are shapes a
// human title does not have; anything cleverer — "looks like a slug", "has no spaces" —
// starts throwing away real titles, and a title is data somebody may have chosen.
const placeholderTitleSQL = `(title IS NOT NULL AND (title ~ '^[a-zA-Z][a-zA-Z0-9+.-]*://' OR title ~ '%[0-9A-Fa-f]{2}'))`

// TitleIsPlaceholder is placeholderTitleSQL in Go, for the importer, which decides
// before a row exists to run a query against.
//
// An empty title is not a placeholder: it is the gap a placeholder pretends not to be,
// and it is already handled — extraction fills it from the page.
func TitleIsPlaceholder(title string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}
	return titleIsURL.MatchString(t) || titleIsEncoded.MatchString(t)
}

var (
	titleIsURL     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://`)
	titleIsEncoded = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)
