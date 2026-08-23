package store

import (
	"context"
	"fmt"
)

// StaleBody names one body that the rules applying to it would no longer produce.
type StaleBody struct {
	// UserID is the reader whose slot is stale, or HouseholdRule for the
	// household's.
	UserID UserID
	ID     ArticleID
}

// StaleBodies finds bodies whose ruleset no longer matches the rule that applies
// to them.
//
// This is the backstop half of "a rule change reprocesses its articles". The other
// half is enqueueing the work when the rule is written, which is fast and is what
// normally does it — and which is also lost entirely if the worker is not running
// at that moment. On this deployment that is not an edge case: the server and the
// worker are separate Deployments, so every rollout, every migration wait and every
// OOM is a window in which a reader can change a rule and have nothing happen.
//
// Every other stage in this pipeline already pairs eager enqueueing with a sweep
// for exactly that reason — DueFeeds, PendingFetch, PendingAssets. This is
// extraction's.
//
// Deliberately over-inclusive rather than exact. It compares against the most
// specific rule matching each host, which is *usually* the effective one but is not
// guaranteed to be when a reader holds rules at two levels; the job it enqueues
// resolves the effective rule properly and skips if the body is already current. An
// unnecessary job costs one query. A missed one costs a reader an article that
// silently never updates, which is the failure this whole mechanism exists to
// prevent.
//
// Suffix matching is done with right() rather than LIKE: `host LIKE '%' || domain`
// would match notexample.com for example.com, and this project has already paid for
// that mistake once with the same shape of query.
func (s *SystemStore) StaleBodies(ctx context.Context, limit int) ([]StaleBody, error) {
	rows, err := s.pool.Query(ctx, `
		WITH applicable AS (
			SELECT DISTINCT ON (COALESCE(r.user_id, 0), a.id)
			       COALESCE(r.user_id, 0) AS user_id,
			       a.id                   AS article_id,
			       r.ruleset_key
			FROM domain_rules r
			JOIN articles a
			  ON a.host = r.domain
			  OR right(a.host, length(r.domain) + 1) = '.' || r.domain
			WHERE COALESCE(a.raw_blob_path, '') <> ''
			ORDER BY COALESCE(r.user_id, 0), a.id, length(r.domain) DESC
		)
		SELECT ap.user_id, ap.article_id
		FROM applicable ap
		LEFT JOIN article_content c
		       ON c.article_id = ap.article_id
		      AND c.is_current
		      AND c.user_id IS NOT DISTINCT FROM NULLIF(ap.user_id, 0)
		-- An immutable body is never regenerated, so it is never stale: an
		-- imported copy may be the only surviving record of a page that is gone,
		-- and a rule cannot conjure a better one out of a page nobody has.
		WHERE (c.id IS NULL OR (NOT c.immutable AND c.ruleset_key IS DISTINCT FROM ap.ruleset_key))
		ORDER BY ap.user_id, ap.article_id
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("finding bodies whose rules have changed: %w", err)
	}
	defer rows.Close()

	var out []StaleBody
	for rows.Next() {
		var b StaleBody
		if err := rows.Scan(&b.UserID, &b.ID); err != nil {
			return nil, fmt.Errorf("scanning a stale body: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ArticlesUnderRule lists the articles a rule applies to, for one owner.
//
// What the eager path enqueues when a rule is written, and what "reprocess this
// host" asks for. Unlike ReextractCandidates it deliberately reaches articles the
// owner has no body of yet: for a reader that is the ordinary state, since they
// read the household's until their rules differ, and selecting only what they
// already have would make the control do nothing the first time it is pressed.
//
// It takes no view on staleness — writing a rule is a statement that you want it
// applied, and the extraction job skips anything already current anyway.
//
// It does take a view on immutability. An article whose current body *for this
// owner* is imported is excluded, because that copy may be the only surviving
// record of a page that is gone and the rule is that nothing automatic replaces
// one. Writing a domain rule is deliberate, but it is a statement about a host
// rather than about that article, and the deliberate act for a single body is
// promoting it.
//
// The raw-page requirement is not that guard, though it hides the need for it: an
// imported article usually has no stored page and drops out for that reason alone.
// One imported and later fetched has both, and would otherwise be silently
// superseded in that reader's view.
func (s *SystemStore) ArticlesUnderRule(
	ctx context.Context, owner *UserID, domain string, limit int,
) ([]ArticleID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id FROM articles a
		LEFT JOIN article_content c
		       ON c.article_id = a.id AND c.is_current
		      AND c.user_id IS NOT DISTINCT FROM $3
		-- The household's body stands in when this owner has none, because that is
		-- what they are reading and therefore what a fork would supersede.
		LEFT JOIN article_content h
		       ON h.article_id = a.id AND h.is_current AND h.user_id IS NULL
		WHERE (a.host = $1 OR right(a.host, length($1) + 1) = '.' || $1)
		  AND COALESCE(a.raw_blob_path, '') <> ''
		  AND NOT COALESCE(c.immutable, h.immutable, false)
		ORDER BY a.id
		LIMIT $2`, domain, limit, owner)
	if err != nil {
		return nil, fmt.Errorf("listing the articles under %q: %w", domain, err)
	}
	defer rows.Close()

	var out []ArticleID
	for rows.Next() {
		var id ArticleID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning an article id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
