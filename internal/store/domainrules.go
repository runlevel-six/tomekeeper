package store

import (
	"context"
	"fmt"
	"strings"
)

// DomainRule is a per-domain extraction override.
//
// Rules are global and admin-only (§2.8): how to extract a site's articles is
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
		VALUES ($1, NULLIF($2, ''), $3, $4, NULLIF($5, ''), NULLIF($6, 0)::numeric, NULLIF($7, ''))
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
