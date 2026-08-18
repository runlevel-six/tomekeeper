package store_test

import (
	"strconv"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// A fractional rate limit survives being stored.
//
// It did not, for the whole life of the feature. `NULLIF($6, 0)::numeric` let
// Postgres infer the parameter's type from the integer literal beside it, so pgx
// encoded 0.5 as an integer, it arrived as 0, and NULLIF turned it into NULL.
// Whole numbers worked, which is why nobody noticed — while fractional values are
// the ones the documentation recommends, since 0.5 is one request every two
// seconds.
//
// Asserted through the store rather than through SQL because both paths matter:
// the value has to be written, and read back as the same number.
func TestDomainRuleKeepsAFractionalRate(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	for _, rate := range []float64{0.5, 0.25, 1, 2.5, 10} {
		domain := "rate-" + strconv.FormatFloat(rate, 'f', -1, 64) + ".example.com"

		if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
			Domain: domain, ContentSelector: ".body", RateLimitRPS: rate,
		}); err != nil {
			t.Fatalf("UpsertDomainRule(%v) = %v", rate, err)
		}

		got, err := s.System().DomainRuleFor(ctx, domain)
		if err != nil {
			t.Fatalf("DomainRuleFor(%v) = %v", rate, err)
		}
		if got.RateLimitRPS != rate {
			t.Errorf("rate %v was stored as %v", rate, got.RateLimitRPS)
		}
	}

	// And no rate at all is still no rate, rather than zero meaning something.
	if err := s.System().UpsertDomainRule(ctx, store.DomainRule{
		Domain: "norate.example.com", ContentSelector: ".body",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}
	got, err := s.System().DomainRuleFor(ctx, "norate.example.com")
	if err != nil {
		t.Fatalf("DomainRuleFor() = %v", err)
	}
	if got.RateLimitRPS != 0 {
		t.Errorf("a rule with no rate reports %v", got.RateLimitRPS)
	}
}
