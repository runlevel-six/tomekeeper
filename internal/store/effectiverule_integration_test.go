package store_test

import (
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

func TestRulesetKey(t *testing.T) {
	base := store.EffectiveRule{ContentSelector: "article.post", StripSelectors: []string{"nav", "aside"}}

	// No rule is a real state, and its key is empty rather than a hash of nothing:
	// a body extracted without a rule must not look stale to a sweep comparing keys.
	if got := (store.EffectiveRule{}).RulesetKey(); got != "" {
		t.Errorf("the empty rule keys as %q, want empty", got)
	}

	// Reordering strip selectors in a form is not a different ruleset. Without this
	// the reprocess control would fire on an edit that changes nothing.
	reordered := store.EffectiveRule{ContentSelector: "article.post", StripSelectors: []string{"aside", "nav"}}
	if base.RulesetKey() != reordered.RulesetKey() {
		t.Error("reordering strip selectors changed the key, so a no-op edit would reprocess a host")
	}

	// Everything that changes the output changes the key.
	for _, other := range []store.EffectiveRule{
		{ContentSelector: "article.other", StripSelectors: []string{"nav", "aside"}},
		{ContentSelector: "article.post", StripSelectors: []string{"nav"}},
		{ContentSelector: "article.post"},
		{StripSelectors: []string{"nav", "aside"}},
	} {
		if base.RulesetKey() == other.RulesetKey() {
			t.Errorf("a different rule keys the same as the base: %+v", other)
		}
	}

	// The fetch half is the household's and does not shape a body, so changing it
	// must not reprocess anything.
	fetchChanged := base
	fetchChanged.RequiresJS = true
	fetchChanged.UserAgent = "something else"
	fetchChanged.RateLimitRPS = 0.25
	if base.RulesetKey() != fetchChanged.RulesetKey() {
		t.Error("a fetch setting changed the ruleset key, so tweaking a rate limit would reprocess a host")
	}
}

func TestEffectiveRuleFor(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	system := s.System()

	bob, err := system.CreateUser(t.Context(), "bob", store.RoleReader)
	if err != nil {
		t.Fatalf("CreateUser() = %v", err)
	}

	// The household's rule: an extraction selector plus the fetch settings only it
	// may hold.
	if err := system.UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: "example.com", ContentSelector: "main.house",
		StripSelectors: []string{"nav"}, RequiresJS: true, RateLimitRPS: 0.5,
	}); err != nil {
		t.Fatalf("UpsertDomainRule(household) = %v", err)
	}

	// With no rule of her own, Alice gets the household's, whole.
	got, err := system.EffectiveRuleFor(t.Context(), alice, "example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor() = %v", err)
	}
	if got.ContentSelector != "main.house" {
		t.Errorf("selector = %q, want the household's", got.ContentSelector)
	}
	if !got.RequiresJS || got.RateLimitRPS != 0.5 {
		t.Error("the household's fetch settings did not come through")
	}
	if got.FromReader {
		t.Error("FromReader is true for a reader with no rule of their own")
	}

	// Alice writes her own.
	if err := system.UpsertReaderRule(t.Context(), alice, store.DomainRule{
		Domain: "example.com", ContentSelector: "main.alice",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}

	got, err = system.EffectiveRuleFor(t.Context(), alice, "example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor() = %v", err)
	}
	if got.ContentSelector != "main.alice" {
		t.Errorf("selector = %q, want Alice's own", got.ContentSelector)
	}
	if !got.FromReader {
		t.Error("FromReader is false for a reader reading their own rule")
	}
	// The fetch half is still the household's — a reader cannot change how a page
	// is retrieved, because it is retrieved once.
	if !got.RequiresJS || got.RateLimitRPS != 0.5 {
		t.Error("a reader's rule displaced the household's fetch settings")
	}

	// Bob is untouched by Alice having written one.
	bobs, err := system.EffectiveRuleFor(t.Context(), bob, "example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor(bob) = %v", err)
	}
	if bobs.ContentSelector != "main.house" {
		t.Errorf("bob's selector = %q, want the household's — he can see Alice's rule", bobs.ContentSelector)
	}
	if bobs.FromReader {
		t.Error("bob's rule reports as his own when it is the household's")
	}

	// And their keys differ, which is what makes their bodies separately stale.
	if got.RulesetKey() == bobs.RulesetKey() {
		t.Error("two readers with different rules produce the same ruleset key")
	}
}

// A reader's own rule wins over the household's even when the household's is the
// more specific domain.
//
// The alternative — most specific domain wins regardless of owner — would mean a
// reader who wrote a rule for a site could have it silently overridden on one of
// its subdomains by a rule they cannot see. Specificity orders a reader's own
// rules against each other; it does not order them against somebody else's.
func TestAReadersRuleWinsOverAMoreSpecificHouseholdRule(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)
	system := s.System()

	if err := system.UpsertDomainRule(t.Context(), store.DomainRule{
		Domain: "blog.example.com", ContentSelector: "main.house-specific",
	}); err != nil {
		t.Fatalf("UpsertDomainRule() = %v", err)
	}
	if err := system.UpsertReaderRule(t.Context(), alice, store.DomainRule{
		Domain: "example.com", ContentSelector: "main.alice-broad",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}

	got, err := system.EffectiveRuleFor(t.Context(), alice, "blog.example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor() = %v", err)
	}
	if got.ContentSelector != "main.alice-broad" {
		t.Errorf("selector = %q, want Alice's own rule to win", got.ContentSelector)
	}

	// Within her own rules, specificity still decides.
	if err := system.UpsertReaderRule(t.Context(), alice, store.DomainRule{
		Domain: "blog.example.com", ContentSelector: "main.alice-specific",
	}); err != nil {
		t.Fatalf("UpsertReaderRule() = %v", err)
	}
	got, err = system.EffectiveRuleFor(t.Context(), alice, "blog.example.com")
	if err != nil {
		t.Fatalf("EffectiveRuleFor() = %v", err)
	}
	if got.ContentSelector != "main.alice-specific" {
		t.Errorf("selector = %q, want her more specific rule", got.ContentSelector)
	}
}

// The database refuses a reader's rule that carries fetch settings, because there
// is nothing per-reader for them to act on: one page, one retrieval.
func TestAReadersRuleCannotCarryFetchSettings(t *testing.T) {
	_, s, alice := dbtest.SetupWithUser(t)

	for _, r := range []store.DomainRule{
		{Domain: "example.com", ContentSelector: "main", RequiresJS: true},
		{Domain: "example.com", ContentSelector: "main", UserAgent: "a crawler"},
		{Domain: "example.com", ContentSelector: "main", RateLimitRPS: 0.5},
	} {
		if err := s.System().UpsertReaderRule(t.Context(), alice, r); err == nil {
			t.Errorf("a reader's rule carrying a fetch setting was accepted: %+v", r)
		}
	}

	// The extraction half alone is fine.
	if err := s.System().UpsertReaderRule(t.Context(), alice, store.DomainRule{
		Domain: "example.com", ContentSelector: "main", StripSelectors: []string{"nav"},
	}); err != nil {
		t.Errorf("a reader's extraction-only rule was refused: %v", err)
	}
}
