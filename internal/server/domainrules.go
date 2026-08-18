package server

import (
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

	form := domainRuleForm{
		Domain:          strings.TrimSpace(r.PostFormValue("domain")),
		ContentSelector: strings.TrimSpace(r.PostFormValue("selector")),
		Strip:           r.PostFormValue("strip"),
		Notes:           strings.TrimSpace(r.PostFormValue("notes")),
		Rate:            strings.TrimSpace(r.PostFormValue("rate")),
		RequiresJS:      r.PostFormValue("requires_js") != "",
	}

	rule, problem := form.rule()
	if problem != "" {
		s.renderDomainRules(w, r, http.StatusBadRequest, form.Domain, form,
			&domainRuleOutcome{Problem: problem})
		return
	}

	if err := s.store.System().UpsertDomainRule(r.Context(), rule); err != nil {
		s.log.Error("saving a domain rule failed", "domain", rule.Domain, "error", err)
		s.renderDomainRules(w, r, http.StatusInternalServerError, form.Domain, form,
			&domainRuleOutcome{Problem: "That rule could not be saved. The log will say why."})
		return
	}

	s.log.Info("saved a domain rule",
		"domain", rule.Domain, "selector", rule.ContentSelector, "strips", len(rule.StripSelectors))

	// The form is cleared and the saved rule is in the table below. What comes
	// next is reprocessing that domain, which is a control on its row.
	s.renderDomainRules(w, r, http.StatusOK, "", domainRuleForm{},
		&domainRuleOutcome{Saved: rule.Domain})
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

	// Version 0 rather than the current extractor version, and this is the whole
	// subtlety of reprocessing after a rule change. Selection is "any version other
	// than this one", so asking for the current version selects nothing: a rule is
	// data that changes between runs of one binary, and no rule edit can bump a
	// constant compiled into it. No body carries version "0", so every body matches.
	queued, err := jobs.QueueReextraction(r.Context(), s.store, s.jobs, jobs.ReextractRequest{
		Version: reprocessEveryVersion,
		Domain:  domain,
	})
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
	rules, err := s.store.System().ListDomainRules(r.Context())
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
	}

	counts, err := s.store.System().ArticlesPerRuleDomain(r.Context())
	if err != nil {
		// A missing count costs a column, not the page.
		s.log.Warn("counting articles per rule domain failed", "error", err)
	}

	for _, rule := range rules {
		page.Rules = append(page.Rules, domainRuleRow{
			DomainRule: rule,
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
