package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Templates and stylesheet are compiled into the binary, so `tome serve` is
// genuinely a single file with nothing to deploy alongside it. There is no
// Node, no bundler, and no build step.
//
//go:embed templates/*.html static/*
var assets embed.FS

// ui holds the parsed templates and the static file handler.
type ui struct {
	pages     map[string]*template.Template
	fragments *template.Template
	static    http.Handler
}

// pageNames are the templates that extend base.html. Listed explicitly rather
// than globbed, so a stray file cannot become a page and a missing one fails at
// startup instead of on the request that needed it.
var pageNames = []string{"login", "stream", "article", "search", "feeds", "attention", "saved"}

// newUI parses every page template.
//
// Each page is its own template set — base plus the shared partials plus that page
// — because html/template resolves blocks per set, and parsing every page into one
// set would leave the last-parsed definition of "content" winning for all of them.
func newUI() (*ui, error) {
	u := &ui{pages: make(map[string]*template.Template, len(pageNames))}

	for _, name := range pageNames {
		t, err := template.New("base.html").Funcs(templateFuncs).ParseFS(assets,
			"templates/base.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parsing the %s template: %w", name, err)
		}
		u.pages[name] = t
	}

	// The partials again on their own, for htmx responses that replace one control
	// rather than a page. Same file, so a fragment cannot drift from the version
	// rendered inside a page.
	fragments, err := template.New("partials.html").Funcs(templateFuncs).ParseFS(assets, "templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("parsing the partials: %w", err)
	}
	u.fragments = fragments

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("opening the static assets: %w", err)
	}
	u.static = http.StripPrefix("/static/", http.FileServerFS(static))

	return u, nil
}

// render writes a page.
//
// The template is executed into a buffer first. Executing straight to the
// ResponseWriter would commit a 200 and a half-written page if a template failed
// partway through, which is the shape of bug that gets diagnosed as "the browser
// truncated it".
func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	t, ok := s.ui.pages[page]
	if !ok {
		s.log.Error("no such template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		s.log.Error("rendering failed", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The archive renders markup from arbitrary websites. Extraction sanitizes it
	// before it is ever stored, but a second, independent restraint costs one
	// header.
	//
	// Everything is 'self' and nothing is 'unsafe-inline'. That is affordable
	// precisely because the assets are vendored and the images are localized: a
	// page that needed a CDN could not have a policy this tight. The two
	// deliberate allowances are data: images, for the small inline images the asset policy
	// leaves in place, and connect-src for htmx's own requests back here.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		// The client hung up mid-write. Nothing to do but say so at a level that
		// does not page anyone.
		s.log.Debug("writing the response failed", "page", page, "error", err)
	}
}

// renderFragment writes one partial, for an htmx swap.
//
// Same buffering as render, and the same security headers: a fragment is still a
// response the browser will interpret.
func (s *Server) renderFragment(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.ui.fragments.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("rendering a fragment failed", "fragment", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		s.log.Debug("writing a fragment failed", "fragment", name, "error", err)
	}
}

// templateFuncs are the handful of helpers the templates need.
//
// Kept deliberately small: logic in a template is logic that cannot be tested, so
// anything beyond formatting belongs in a handler.
var templateFuncs = template.FuncMap{
	// "3 minutes ago", for a reader scanning a stream. Dates are what a reader
	// actually wants further back, so this switches over rather than reporting
	// "4,102 hours ago".
	"since": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return plural(int(d.Minutes()), "minute") + " ago"
		case d < 24*time.Hour:
			return plural(int(d.Hours()), "hour") + " ago"
		case d < 14*24*time.Hour:
			return plural(int(d.Hours()/24), "day") + " ago"
		default:
			return t.Format("2 January 2006")
		}
	},
	// Reading time at 220 words per minute, the usual figure for prose.
	"readingTime": func(words int) string {
		if words <= 0 {
			return ""
		}
		minutes := max(1, (words+109)/220)
		return plural(minutes, "minute") + " read"
	},
	"plural": plural,
	"add":    func(a, b int) int { return a + b },

	// A heading for an article that may not have one yet.
	//
	// A page saved by hand has no title until the worker fetches and extracts it,
	// and "(untitled)" for every row in a fresh reading list is useless — the
	// reader cannot tell which link is which. The URL is what they pasted, so it
	// is what they recognize; the scheme is dropped because it never distinguishes
	// two rows.
	"displayTitle": func(title, canonical string) string {
		if title != "" {
			return title
		}
		if trimmed := strings.TrimPrefix(strings.TrimPrefix(canonical, "https://"), "http://"); trimmed != "" {
			return trimmed
		}
		return "(untitled)"
	},

	// Turns a search snippet into HTML safely.
	//
	// The snippet is article *text* with sentinels around the matched terms, so it
	// can contain anything a writer typed — including a literal <script> in a post
	// about markup. Escape everything first, then substitute the sentinels for real
	// tags: the highlight is the only markup that can survive, and whatever the
	// article contained stays visible-but-inert.
	"snippet": func(s string) template.HTML {
		escaped := template.HTMLEscapeString(s)
		escaped = strings.ReplaceAll(escaped, template.HTMLEscapeString(store.HighlightStart), "<mark>")
		escaped = strings.ReplaceAll(escaped, template.HTMLEscapeString(store.HighlightEnd), "</mark>")
		return template.HTML(escaped) //nolint:gosec // escaped above; only <mark> is reintroduced
	},

	// Projects the article page onto the shared "actions" partial.
	"articleActions": func(p articlePage) actions {
		return actions{ArticleID: p.Article.ID, Read: p.Read, Starred: p.Starred, Kept: p.Kept}
	},

	// Projects a stream row onto the shape the shared "actions" partial takes, so
	// the control inside a row and the one returned by htmx are the same template
	// with the same data rather than two that have to be kept in step.
	"actionsOf": func(it store.StreamItem) actions {
		return actions{ArticleID: it.ArticleID, Read: it.Read, Starred: it.Starred, Kept: it.Kept}
	},
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// staticMaxAge is how long a browser may cache the stylesheet.
//
// Short, because the asset name carries no hash: a long cache would leave a
// changed stylesheet stuck in browsers after an upgrade, and this file is a few
// kilobytes served to one reader.
const staticMaxAge = 5 * time.Minute

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(staticMaxAge.Seconds())))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.ui.static.ServeHTTP(w, r)
}
