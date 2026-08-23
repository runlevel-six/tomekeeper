package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EffectiveRule is the rule that actually shapes one reader's extraction of one
// host: their own where they have written one, the household's otherwise.
//
// The two halves of a domain rule belong to different owners, and this type is
// where that split is resolved:
//
//   - **Extraction** — the content selector and the strip selectors — decides how a
//     stored page becomes a body. Two readers may hold different ones and each get
//     their own extraction, because a body is per-reader.
//   - **Fetching** — requires_js, the user agent, the rate limit — decides how the
//     page is *retrieved*, and there is one retrieval. The household owns it, and a
//     reader's rule may not carry those fields at all; the database refuses one that
//     does.
//
// So a reader can decide what their copy of a page says. They cannot decide that
// the archive fetches it twice.
type EffectiveRule struct {
	// Extraction, from the reader's rule when they have one.
	ContentSelector string
	StripSelectors  []string

	// Fetching, always the household's.
	RequiresJS   bool
	UserAgent    string
	RateLimitRPS float64

	// Domain is the rule that matched, for reporting. Empty when none did.
	Domain string

	// FromReader says the extraction half came from this reader rather than from
	// the household — which is what an explanation of their body has to be able to
	// say.
	FromReader bool
}

// RulesetKey identifies the extraction half of a rule.
//
// Stored on each body so a later sweep can tell what produced it. Only the
// extraction fields go in: a change to the rate limit or the user agent alters how
// the page is fetched, and a page already on disk does not need re-extracting
// because of it. Including them would make every rate-limit tweak reprocess a host
// for no change in output.
//
// Empty for no rule, which is a real state and distinct from a rule that selects
// nothing. Strip selectors are sorted first, so reordering them in the form does
// not read as a different ruleset and trigger a reprocess that changes nothing.
//
// A hash rather than the selectors themselves: this is compared, never parsed, and
// a selector can be long enough that storing it on every body would cost more than
// the bodies.
func (r EffectiveRule) RulesetKey() string {
	if r.ContentSelector == "" && len(r.StripSelectors) == 0 {
		return ""
	}

	strips := append([]string(nil), r.StripSelectors...)
	sort.Strings(strips)

	// A separator that cannot appear in a CSS selector, so two different rules
	// cannot serialize to the same string.
	h := sha256.Sum256([]byte(r.ContentSelector + "\x00" + strings.Join(strips, "\x00")))
	return hex.EncodeToString(h[:8])
}

// EffectiveRuleFor returns the rule shaping one reader's extraction of a host.
//
// One query rather than two lookups and a merge in Go: the reader's rule and the
// household's are found together, most specific domain first, and the reader's wins
// where both exist. Doing it in two round trips would invite a caller to use one
// without the other, which is how a reader's selector gets applied with the
// household's fetch settings dropped — or worse, the reverse.
//
// On SystemStore because it reads across owners by design: resolving what a reader
// sees requires the household's row as well as theirs.
func (s *SystemStore) EffectiveRuleFor(ctx context.Context, userID UserID, host string) (EffectiveRule, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return EffectiveRule{}, fmt.Errorf("host must not be empty")
	}

	rows, err := s.pool.Query(ctx, `
		SELECT user_id IS NOT NULL AS mine,
		       domain, COALESCE(content_selector, ''), COALESCE(strip_selectors, '{}'),
		       requires_js, COALESCE(user_agent, ''), COALESCE(rate_limit_rps, 0)::float8
		FROM domain_rules
		WHERE domain = ANY($2) AND (user_id = $1 OR user_id IS NULL)
		-- Most specific domain first, and within a domain the reader's own rule
		-- ahead of the household's.
		ORDER BY length(domain) DESC, (user_id IS NOT NULL) DESC`,
		userID, domainCandidates(host))
	if err != nil {
		return EffectiveRule{}, fmt.Errorf("looking up the rules for %q: %w", host, err)
	}
	defer rows.Close()

	var (
		out       EffectiveRule
		haveMine  bool
		haveHouse bool
	)
	for rows.Next() {
		var (
			mine      bool
			domain    string
			selector  string
			strips    []string
			js        bool
			agent     string
			rateLimit float64
		)
		if err := rows.Scan(&mine, &domain, &selector, &strips, &js, &agent, &rateLimit); err != nil {
			return EffectiveRule{}, fmt.Errorf("scanning a domain rule: %w", err)
		}

		// The first row of each kind wins, because the ordering already put the
		// most specific domain first. Later rows are less specific matches.
		if mine && !haveMine {
			haveMine = true
			out.ContentSelector, out.StripSelectors = selector, strips
			out.Domain, out.FromReader = domain, true
		}
		if !mine && !haveHouse {
			haveHouse = true
			out.RequiresJS, out.UserAgent, out.RateLimitRPS = js, agent, rateLimit
			if !haveMine {
				out.ContentSelector, out.StripSelectors = selector, strips
				out.Domain = domain
			}
		}
	}
	return out, rows.Err()
}

// ErrReaderRuleFetchSettings means a reader's rule tried to carry a setting that
// decides how a page is fetched.
var ErrReaderRuleFetchSettings = errors.New(
	"a reader's rule may set the content and strip selectors only; " +
		"how a page is fetched is the household's")

// UpsertReaderRule writes one reader's own rule for a domain.
//
// Refused here as well as by the check constraint, so a caller gets a sentence
// rather than a SQLSTATE — and so the reason is written down somewhere a reader of
// this package will find it. The constraint is what makes it true; this is what
// makes it explicable.
func (s *SystemStore) UpsertReaderRule(ctx context.Context, userID UserID, r DomainRule) error {
	if r.RequiresJS || r.UserAgent != "" || r.RateLimitRPS != 0 {
		return ErrReaderRuleFetchSettings
	}

	domain := strings.ToLower(strings.TrimSpace(r.Domain))
	if domain == "" {
		return fmt.Errorf("domain must not be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO domain_rules (user_id, domain, content_selector, strip_selectors, notes)
		VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''))
		ON CONFLICT (domain, COALESCE(user_id, 0)) DO UPDATE SET
			content_selector = EXCLUDED.content_selector,
			strip_selectors  = EXCLUDED.strip_selectors,
			notes            = EXCLUDED.notes`,
		userID, domain, r.ContentSelector, r.StripSelectors, r.Notes)
	if err != nil {
		return fmt.Errorf("saving %d's rule for %s: %w", userID, domain, err)
	}
	return nil
}

// DeleteReaderRule removes one reader's rule, leaving the household's in place.
//
// Deleting yours is how you go back to what everybody else gets, which is why this
// cannot touch a row with no owner.
func (s *SystemStore) DeleteReaderRule(ctx context.Context, userID UserID, domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM domain_rules WHERE domain = $2 AND user_id = $1`, userID, domain)
	if err != nil {
		return fmt.Errorf("deleting %d's rule for %s: %w", userID, domain, err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
