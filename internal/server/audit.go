package server

import (
	"net/http"
	"net/url"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// auditPage reports stored bodies that may not be the article they claim to be.
//
// The attention queue answers "what did not arrive". This answers the harder
// question underneath it — "what arrived and is wrong" — which nothing asked before
// a re-fetch stored a cookie consent dialog as a 410-word article and took it out of
// that queue by succeeding. Everything here is a judgement call and some of it is
// meant to be a false alarm, which is why it reports and never acts.
//
// **Its own page rather than a section of /attention**, which is a decision the
// measurement made rather than a preference. The three lenses cost about 1.1
// seconds against a 2,264-body archive, almost all of it the title lens, and they
// grow with the archive. Folding them into /attention would have charged that to
// every visit — including the visits where the section turns out to be empty and
// nothing is shown at all. So the link is always present and the work happens when
// somebody asks for it. The cost of that choice is the suppression-at-zero the
// design wanted: a page you deliberately opened may say "nothing to look at", which
// is what the command it comes from has always done.
type auditPage struct {
	pageData

	Suspect []auditSuspectRow
	Shared  []store.SharedBody
	Titles  []auditTitleRow

	// Findings is the total across the three lenses, so the page can lead with
	// whether there is anything to read.
	Findings int

	// Limit is the most rows any one lens will show, named so a lens that reached
	// it can say it was cut off rather than appearing to be complete.
	Limit int

	// Reprocessable says whether a whole-archive reprocess can be offered as the
	// remedy for a title that is a URL, which needs a job queue.
	Reprocessable bool
}

// auditSuspectRow is a suspect body with the way out attached.
//
// The remedy for extraction having chosen the wrong block of a page is a rule for
// that host, so the row carries the link to write one — the same move that turned
// the attention queue from a list of complaints into a list of next steps.
type auditSuspectRow struct {
	store.SuspectBody

	Host     string
	RulePath string
}

// auditTitleRow is a placeholder title, and which of two remedies it wants.
//
// The distinction is the whole reason this lens separates them: an article with a
// body has a page to take a real title from and needs re-extraction, while a
// bodyless one has nothing to read a title out of and needs a fetch first. Offering
// the wrong one is offering something that cannot work.
type auditTitleRow struct {
	store.PlaceholderTitle

	Host string
}

// auditLimit is the most findings any one lens reports.
//
// The same default the command uses. A cap rather than a page of pagination
// controls, because this is a list somebody reads through once in maintenance mode
// and not a stream: fifty rows is already more than anybody works through in a
// sitting, and a lens that finds hundreds is telling you something about the archive
// rather than about fifty articles.
const auditLimit = 50

// handleAudit runs the three lenses over what this reader can see.
//
// Scoped, and that scoping is the reason this waited for tenancy rather than
// shipping with the command. The command's queries are archive-wide, which is right
// for an operator maintaining an archive; a page is not, and every lens here has to
// look only at this reader's articles and at the body each of those shows them —
// their own extraction where their rules produced one, the household's otherwise.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	page := auditPage{
		pageData:      s.pageData(r, "attention"),
		Limit:         auditLimit,
		Reprocessable: s.jobs != nil,
	}

	suspect, err := s.store.SuspectBodiesFor(r.Context(), userID, auditLimit)
	if err != nil {
		s.log.Error("auditing bodies against their titles failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, b := range suspect {
		row := auditSuspectRow{SuspectBody: b}
		if host, ok := hostOf(b.URL); ok {
			row.Host = host
			row.RulePath = "/domain-rules?edit=" + url.QueryEscape(host)
		}
		page.Suspect = append(page.Suspect, row)
	}

	shared, err := s.store.SharedBodiesFor(r.Context(), userID, auditLimit)
	if err != nil {
		s.log.Error("auditing bodies for duplicates failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	page.Shared = shared

	titles, err := s.store.PlaceholderTitlesFor(r.Context(), userID, auditLimit)
	if err != nil {
		s.log.Error("auditing titles failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, t := range titles {
		row := auditTitleRow{PlaceholderTitle: t}
		if host, ok := hostOf(t.URL); ok {
			row.Host = host
		}
		page.Titles = append(page.Titles, row)
	}

	page.Findings = len(page.Suspect) + len(page.Shared) + len(page.Titles)

	s.render(w, http.StatusOK, "audit", page)
}
