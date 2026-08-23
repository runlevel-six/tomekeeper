package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// domainRulesPage is the extraction overrides, and the form that writes one.
//
// **This is admin surface, not reader surface.** Domain rules are global — how to
// extract a site's articles is a technical fact about that site, identical for
// every reader — so everything here reaches through Store.System() and is
// deliberately outside the per-user scoping the rest of the interface lives by.
// That is correct for a single-user archive and is exactly what multi-user work
// will have to gate: these routes need a role check before a second person exists,
// and the greppable marker for that is the System() calls in this file.
type domainRulesPage struct {
	pageData

	Rules []domainRuleRow

	// Editing is the domain whose rule is loaded into the form, empty when the
	// form is blank and ready for a new one.
	Editing string

	// Form is what the fields hold, which is what was submitted when something was
	// wrong with it and the stored rule when one is being edited.
	Form domainRuleForm

	// ReprocessAvailable says whether re-extraction can be queued from here, which
	// needs a job queue. Without one the control says so rather than lying.
	ReprocessAvailable bool

	// ExtractorVersion is what this build extracts at, shown so that "reprocess"
	// has a stated meaning rather than being a button that does something
	// unspecified.
	ExtractorVersion string

	// Outcome reports what the last action did.
	Outcome *domainRuleOutcome

	// CanSetHousehold says whether this reader may write the rule everybody gets.
	//
	// Administrators only. The distinction is shown rather than implied, because
	// "did I just change this for everyone or only for me" is the question a rule
	// form must never leave ambiguous.
	CanSetHousehold bool

	// ForHousehold is what the ownership control holds — on by default for an
	// administrator, so saving a rule keeps doing what it did when there was one
	// reader.
	ForHousehold bool
}

// domainRuleRow is one rule as the table shows it.
type domainRuleRow struct {
	store.DomainRule

	// Articles is how many stored articles come from this host, which is the
	// number that says whether a rule is worth writing and how much reprocessing
	// it implies.
	Articles int64

	// Strip is the strip selectors joined for display, since the form takes them
	// one per line.
	Strip string

	// Mine says this is the reader's own rule rather than the household's. The
	// table shows both, and a row that did not say which was which would make a
	// reader think they had changed something they had not.
	Mine bool
}

// domainRuleForm is the editable shape of a rule.
//
// Strings rather than the store's types, because a form holds what somebody typed
// including the version of it that will not parse — a rate of "one per minute"
// has to survive long enough to be shown back to them with a complaint.
type domainRuleForm struct {
	Domain          string
	ContentSelector string
	Strip           string
	Notes           string
	Rate            string
	RequiresJS      bool

	// ForHousehold asks for the rule everybody gets rather than this reader's own.
	// Ignored for anybody who is not an administrator.
	ForHousehold bool
}

// domainRuleOutcome is what the page says after a change.
type domainRuleOutcome struct {
	Problem string

	// Saved, Deleted and Queued name what happened, so the page can say it in
	// terms of the domain rather than as a generic success.
	Saved   string
	Deleted string

	Reprocessed string
	Queued      int

	// ForHousehold says the save changed what everybody gets, which the page has
	// to state rather than leave to be inferred from a checkbox that has since
	// been cleared.
	ForHousehold bool
}

func (s *Server) handleDomainRules(w http.ResponseWriter, r *http.Request) {
	form := domainRuleForm{}
	editing := strings.TrimSpace(r.URL.Query().Get("edit"))

	if editing != "" {
		// The domain is filled in either way, so that following "edit" for a
		// subdomain with no rule of its own lands on a form ready to create one
		// rather than on a blank page.
		form.Domain = editing

		rule, err := s.store.System().DomainRuleFor(r.Context(), editing)
		switch {
		case err != nil:
			s.log.Info("no rule to edit", "domain", editing, "error", err)
		case rule.Domain == editing:
			// Only an exact match loads into the form. DomainRuleFor walks up to a
			// parent domain, and editing blog.example.com must not silently open
			// example.com's rule and save it back under the child — which would
			// copy a rule rather than edit one, and leave the parent's unchanged.
			form = formFor(rule)
		}
	}

	s.renderDomainRules(w, r, http.StatusOK, editing, form, nil)
}

// handleSaveDomainRule writes one rule.
func (s *Server) handleSaveDomainRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	account := signedInAccount(r)
	form := domainRuleForm{
		Domain:          strings.TrimSpace(r.PostFormValue("domain")),
		ContentSelector: strings.TrimSpace(r.PostFormValue("selector")),
		Strip:           r.PostFormValue("strip"),
		Notes:           strings.TrimSpace(r.PostFormValue("notes")),
		Rate:            strings.TrimSpace(r.PostFormValue("rate")),
		RequiresJS:      r.PostFormValue("requires_js") != "",
		// Only an administrator may write the rule everybody gets. Read from the
		// account rather than from the form alone, so a hand-crafted POST cannot
		// change what other readers see.
		ForHousehold: account.IsAdmin() && r.PostFormValue("for_household") != "",
	}

	rule, problem := form.rule()
	if problem != "" {
		s.renderDomainRules(w, r, http.StatusBadRequest, form.Domain, form,
			&domainRuleOutcome{Problem: problem})
		return
	}

	owner := account.ID
	var err error
	if form.ForHousehold {
		owner = store.HouseholdRule
		err = s.store.System().UpsertDomainRule(r.Context(), rule)
	} else {
		err = s.store.System().UpsertReaderRule(r.Context(), account.ID, rule)
	}
	if err != nil {
		if errors.Is(err, store.ErrReaderRuleFetchSettings) {
			s.renderDomainRules(w, r, http.StatusBadRequest, form.Domain, form,
				&domainRuleOutcome{Problem: "Whether a page needs a browser, and how fast it is " +
					"fetched, are the same for everyone — the page is fetched once. Only the " +
					"selectors can be yours alone."})
			return
		}
		s.log.Error("saving a domain rule failed", "domain", rule.Domain, "error", err)
		s.renderDomainRules(w, r, http.StatusInternalServerError, form.Domain, form,
			&domainRuleOutcome{Problem: "That rule could not be saved. The log will say why."})
		return
	}

	s.log.Info("saved a domain rule",
		"domain", rule.Domain, "selector", rule.ContentSelector, "strips", len(rule.StripSelectors),
		"for_household", form.ForHousehold, "user_id", account.ID)

	// Saving a rule is a statement that the articles it covers should be
	// reconsidered, so this queues them rather than leaving it to a second button.
	// A failure here does not fail the save: the rule is stored, and the sweep that
	// runs every minute will find the same work.
	outcome := &domainRuleOutcome{Saved: rule.Domain, ForHousehold: form.ForHousehold}
	if s.jobs != nil {
		queued, qErr := jobs.QueueRuleExtraction(r.Context(), s.store, s.jobs, owner, rule.Domain)
		if qErr != nil {
			s.log.Warn("queueing extraction after a rule change failed; the sweep will pick it up",
				"domain", rule.Domain, "error", qErr)
		}
		outcome.Queued = queued
	}

	s.renderDomainRules(w, r, http.StatusOK, "", domainRuleForm{}, outcome)
}

// handleDeleteDomainRule removes one rule.
func (s *Server) handleDeleteDomainRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	domain := strings.TrimSpace(r.PostFormValue("domain"))
	if domain == "" {
		http.NotFound(w, r)
		return
	}

	removed, err := s.store.System().DeleteDomainRule(r.Context(), domain)
	if err != nil {
		s.log.Error("deleting a domain rule failed", "domain", domain, "error", err)
		s.renderDomainRules(w, r, http.StatusInternalServerError, "", domainRuleForm{},
			&domainRuleOutcome{Problem: "That rule could not be removed. The log will say why."})
		return
	}
	if !removed {
		http.NotFound(w, r)
		return
	}

	s.log.Info("deleted a domain rule", "domain", domain)

	// Deliberately does not reprocess. Removing a rule changes how the site will be
	// extracted, and whether the articles already stored under it should go back to
	// the heuristics is a separate decision — one the reprocess control on this page
	// makes explicit rather than silent.
	s.renderDomainRules(w, r, http.StatusOK, "", domainRuleForm{},
		&domainRuleOutcome{Deleted: domain})
}

// handleReprocessDomain queues re-extraction for one host.
//
// This is the second half of writing a rule, and until now it lived only in
// `tome reextract --domain X --target-version 0` — which meant the routine
// maintenance the failed-fetch queue exists to drive ended at a shell prompt.
// It queues exactly what that command queues, through the same function.
func (s *Server) handleReprocessDomain(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form could not be read", http.StatusBadRequest)
		return
	}

	domain := strings.TrimSpace(r.PostFormValue("domain"))
	if domain == "" {
		http.NotFound(w, r)
		return
	}

	if s.jobs == nil {
		s.renderDomainRules(w, r, http.StatusOK, "", domainRuleForm{}, &domainRuleOutcome{
			Problem: "This instance cannot queue re-extraction, because it has no job queue " +
				"configured. `tome reextract --target-version 0 --domain " + domain + "` does the same thing.",
		})
		return
	}

	// Whose bodies this reprocesses follows whose rule the row is. Reprocessing the
	// household's on a reader's behalf would change what everybody reads from a
	// control that says nothing about doing so.
	forHousehold := r.PostFormValue("for_household") != "" && signedInAccount(r).IsAdmin()

	// The two are different questions, not one with a parameter.
	//
	// For the household: bring bodies forward that are at some other version.
	// Selection is "any version other than this one", so version 0 is how it says
	// "whatever version they are, do them again" — a rule is data that changes
	// between runs of one binary, and no rule edit can bump a constant compiled
	// into it. Imported bodies are provably excluded by that query's own WHERE
	// clause.
	//
	// For a reader: apply my rules to this host. That has to reach articles they
	// have no body of yet, because having none is the ordinary state — they read
	// the household's until their rules differ. Selecting only what they already
	// have would make the control do nothing the first time it is pressed, which is
	// exactly when somebody presses it.
	var (
		queued int
		err    error
	)
	if forHousehold {
		queued, err = jobs.QueueReextraction(r.Context(), s.store, s.jobs, jobs.ReextractRequest{
			Version: reprocessEveryVersion,
			Domain:  domain,
			Owner:   store.Household(),
		})
	} else {
		queued, err = jobs.QueueRuleExtraction(
			r.Context(), s.store, s.jobs, signedInAccount(r).ID, domain)
	}
	if err != nil {
		s.log.Error("queueing re-extraction failed", "domain", domain, "error", err)
		s.renderDomainRules(w, r, http.StatusInternalServerError, "", domainRuleForm{},
			&domainRuleOutcome{
				Problem: "Re-extraction could not be queued. The log will say why. " +
					"Anything already queued is safe to leave; re-running this is harmless.",
			})
		return
	}

	s.log.Info("queued re-extraction from the web interface", "domain", domain, "articles", queued)

	s.renderDomainRules(w, r, http.StatusOK, "", domainRuleForm{},
		&domainRuleOutcome{Reprocessed: domain, Queued: queued})
}

// reprocessEveryVersion is the target version that selects every mutable body.
//
// No extraction has ever recorded "0", and the predicate is "other than this", so
// this is how a reprocess says "whatever version they are, do them again". Named
// rather than inlined because a bare "0" at the call site reads like a count.
const reprocessEveryVersion = "0"

// renderDomainRules draws the page.
func (s *Server) renderDomainRules(w http.ResponseWriter, r *http.Request, status int,
	editing string, form domainRuleForm, outcome *domainRuleOutcome,
) {
	account := signedInAccount(r)

	// This reader's rules and the household's, never another reader's: showing
	// Alice that Bob has a rule for a host tells her he reads it.
	rules, err := s.store.System().RulesVisibleTo(r.Context(), account.ID)
	if err != nil {
		s.log.Error("listing domain rules failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page := domainRulesPage{
		pageData:           s.pageData(r, "domain-rules"),
		Editing:            editing,
		Form:               form,
		ReprocessAvailable: s.jobs != nil,
		ExtractorVersion:   extract.Version,
		Outcome:            outcome,
		CanSetHousehold:    account.IsAdmin(),
		ForHousehold:       form.ForHousehold,
	}

	counts, err := s.store.System().ArticlesPerRuleDomain(r.Context())
	if err != nil {
		// A missing count costs a column, not the page.
		s.log.Warn("counting articles per rule domain failed", "error", err)
	}

	for _, rule := range rules {
		page.Rules = append(page.Rules, domainRuleRow{
			DomainRule: rule.DomainRule,
			Mine:       rule.Mine,
			Articles:   counts[rule.Domain],
			Strip:      strings.Join(rule.StripSelectors, ", "),
		})
	}

	s.render(w, status, "domainrules", page)
}

// formFor projects a stored rule into the form.
func formFor(rule store.DomainRule) domainRuleForm {
	form := domainRuleForm{
		Domain:          rule.Domain,
		ContentSelector: rule.ContentSelector,
		Strip:           strings.Join(rule.StripSelectors, "\n"),
		Notes:           rule.Notes,
		RequiresJS:      rule.RequiresJS,
	}
	if rule.RateLimitRPS > 0 {
		form.Rate = strconv.FormatFloat(rule.RateLimitRPS, 'g', -1, 64)
	}
	return form
}

// rule turns the submitted form into a rule, or explains why it cannot.
//
// The explanations are the point of doing this here rather than in the store: a
// selector that matches nothing is a mistake somebody can only find out about by
// trying it, but a domain with a scheme on it or a rate that is not a number are
// mistakes the form can name immediately.
func (f domainRuleForm) rule() (store.DomainRule, string) {
	rule := store.DomainRule{
		Domain:          strings.ToLower(strings.TrimSpace(f.Domain)),
		ContentSelector: f.ContentSelector,
		Notes:           f.Notes,
		RequiresJS:      f.RequiresJS,
	}

	if rule.Domain == "" {
		return store.DomainRule{}, "A rule needs a domain."
	}
	// A pasted URL is the obvious mistake, and it is worth correcting rather than
	// refusing: somebody pasting https://example.com/blog/ means example.com.
	if trimmed, ok := hostOf(rule.Domain); ok {
		rule.Domain = trimmed
	} else {
		return store.DomainRule{}, "That does not look like a domain. Give a host such as example.com, " +
			"which will also cover its subdomains."
	}

	for _, line := range strings.Split(f.Strip, "\n") {
		if selector := strings.TrimSpace(line); selector != "" {
			rule.StripSelectors = append(rule.StripSelectors, selector)
		}
	}

	if f.Rate != "" {
		rate, err := strconv.ParseFloat(f.Rate, 64)
		if err != nil || rate <= 0 {
			return store.DomainRule{}, "The rate must be a number of requests per second — " +
				"0.5 is one request every two seconds."
		}
		rule.RateLimitRPS = rate
	}

	if rule.ContentSelector == "" && len(rule.StripSelectors) == 0 &&
		!rule.RequiresJS && rule.RateLimitRPS == 0 {
		return store.DomainRule{}, "That rule would do nothing. Give a content selector, " +
			"something to strip, a rate, or mark the site as needing JavaScript."
	}

	return rule, ""
}

// hostOf reduces what somebody typed to a bare host.
func hostOf(raw string) (string, bool) {
	host := raw
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, "?")
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host, _, _ = strings.Cut(host, ":")
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")

	if host == "" || strings.ContainsAny(host, " \t/\\") || !strings.Contains(host, ".") {
		return "", false
	}
	return host, true
}
