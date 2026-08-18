package store

import (
	"context"
	"fmt"
	"strings"
)

// DomainRule is a per-domain extraction override.
//
// Rules are global and admin-only: how to extract a site's articles is
// a technical fact about that site, identical for every reader, and there is
// no version of it that belongs to one user. They live on SystemStore for that
// reason, not as an oversight.
type DomainRule struct {
	Domain          string
	ContentSelector string
	StripSelectors  []string
	RequiresJS      bool
	UserAgent       string
	RateLimitRPS    float64
	Notes           string
}

// DomainRuleFor returns the rule covering a host.
//
// Lookup walks up the domain: a rule for example.com applies to
// blog.example.com unless that subdomain has a rule of its own. Sites are
// usually built once and deployed across subdomains, so a rule written for the
// parent is nearly always right for the child, and requiring one rule per
// subdomain would make the failed-fetch queue tedious to drain.
func (s *SystemStore) DomainRuleFor(ctx context.Context, host string) (DomainRule, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return DomainRule{}, fmt.Errorf("host must not be empty")
	}

	candidates := domainCandidates(host)

	var r DomainRule
	var selectors []string
	err := s.pool.QueryRow(ctx, `
		SELECT domain, COALESCE(content_selector, ''), COALESCE(strip_selectors, '{}'),
		       requires_js, COALESCE(user_agent, ''),
		       COALESCE(rate_limit_rps, 0)::float8, COALESCE(notes, '')
		FROM domain_rules
		WHERE domain = ANY($1)
		-- The most specific match wins: blog.example.com beats example.com.
		ORDER BY length(domain) DESC
		LIMIT 1`, candidates,
	).Scan(&r.Domain, &r.ContentSelector, &selectors, &r.RequiresJS,
		&r.UserAgent, &r.RateLimitRPS, &r.Notes)
	if err != nil {
		return DomainRule{}, err
	}
	r.StripSelectors = selectors
	return r, nil
}

// domainCandidates returns a host and each of its parent domains.
//
// The public suffix is not consulted, so "co.uk" is a candidate for
// "bbc.co.uk". That is harmless: a rule is only found if someone deliberately
// wrote one for that exact string, and nobody writes a rule for a public
// suffix.
func domainCandidates(host string) []string {
	parts := strings.Split(host, ".")
	candidates := make([]string, 0, len(parts))

	for i := range parts {
		if candidate := strings.Join(parts[i:], "."); strings.Contains(candidate, ".") || i == 0 {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// ListDomainRules returns every rule, ordered by domain.
func (s *SystemStore) ListDomainRules(ctx context.Context) ([]DomainRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT domain, COALESCE(content_selector, ''), COALESCE(strip_selectors, '{}'),
		       requires_js, COALESCE(user_agent, ''),
		       COALESCE(rate_limit_rps, 0)::float8, COALESCE(notes, '')
		FROM domain_rules ORDER BY domain`)
	if err != nil {
		return nil, fmt.Errorf("listing domain rules: %w", err)
	}
	defer rows.Close()

	var out []DomainRule
	for rows.Next() {
		var r DomainRule
		var selectors []string
		if err := rows.Scan(&r.Domain, &r.ContentSelector, &selectors, &r.RequiresJS,
			&r.UserAgent, &r.RateLimitRPS, &r.Notes); err != nil {
			return nil, fmt.Errorf("scanning domain rule: %w", err)
		}
		r.StripSelectors = selectors
		out = append(out, r)
	}
	return out, rows.Err()
}

// ArticlesPerRuleDomain counts stored articles per rule domain.
//
// Keyed by the rule's domain and counting its subdomains with it, matching how a
// rule applies and — more importantly — matching exactly what reprocessing that
// domain would select. A count shown next to a reprocess control that disagreed
// with what the control does would be worse than no count.
//
// The host is derived from url_canonical the same way ReextractCandidates derives
// it, and compared as a host rather than with a LIKE over the whole URL:
// `LIKE '%example.com%'` also matches notexample.com and, worse,
// evil.com/?ref=example.com.
//
// Admin surface, like everything else about rules: this counts across every
// reader's articles, which is right for a global rule and is one of the places a
// multi-user build has to grow a role check.
func (s *SystemStore) ArticlesPerRuleDomain(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		WITH hosts AS (
			SELECT split_part(split_part(split_part(a.url_canonical, '://', 2), '/', 1), ':', 1) AS host
			FROM articles a
		)
		SELECT r.domain, count(h.host)
		FROM domain_rules r
		LEFT JOIN hosts h ON h.host = r.domain OR h.host LIKE '%.' || r.domain
		GROUP BY r.domain`)
	if err != nil {
		return nil, fmt.Errorf("counting articles per rule domain: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var (
			domain string
			n      int64
		)
		if err := rows.Scan(&domain, &n); err != nil {
			return nil, fmt.Errorf("scanning a domain count: %w", err)
		}
		counts[domain] = n
	}
	return counts, rows.Err()
}

// UpsertDomainRule creates or replaces a rule.
func (s *SystemStore) UpsertDomainRule(ctx context.Context, r DomainRule) error {
	domain := strings.ToLower(strings.TrimSpace(r.Domain))
	if domain == "" {
		return fmt.Errorf("domain must not be empty")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO domain_rules (
			domain, content_selector, strip_selectors, requires_js,
			user_agent, rate_limit_rps, notes
		)
		-- The rate is cast before the comparison, not after. Written as
		-- NULLIF($6, 0)::numeric, Postgres infers the parameter's type from the
		-- integer literal it is compared with, so pgx sent 0.5 as an integer, it
		-- arrived as 0, and NULLIF turned it into NULL. Fractional rates — the
		-- useful ones, since 0.5 is one request every two seconds — were silently
		-- discarded while whole numbers worked, which is why it went unnoticed.
		VALUES ($1, NULLIF($2, ''), $3, $4, NULLIF($5, ''), NULLIF($6::numeric, 0), NULLIF($7, ''))
		ON CONFLICT (domain) DO UPDATE SET
			content_selector = EXCLUDED.content_selector,
			strip_selectors  = EXCLUDED.strip_selectors,
			requires_js      = EXCLUDED.requires_js,
			user_agent       = EXCLUDED.user_agent,
			rate_limit_rps   = EXCLUDED.rate_limit_rps,
			notes            = EXCLUDED.notes`,
		domain, r.ContentSelector, r.StripSelectors, r.RequiresJS,
		r.UserAgent, r.RateLimitRPS, r.Notes)
	if err != nil {
		return fmt.Errorf("saving the rule for %s: %w", domain, err)
	}
	return nil
}

// DeleteDomainRule removes a rule, reporting whether one was there.
func (s *SystemStore) DeleteDomainRule(ctx context.Context, domain string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM domain_rules WHERE domain = $1`,
		strings.ToLower(strings.TrimSpace(domain)))
	if err != nil {
		return false, fmt.Errorf("deleting the rule for %s: %w", domain, err)
	}
	return tag.RowsAffected() > 0, nil
}
