package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

// Templates and stylesheet are compiled into the binary, so `tome serve` is
// genuinely a single file with nothing to deploy alongside it (§5.7). There is no
// Node, no bundler, and no build step.
//
//go:embed templates/*.html static/*
var assets embed.FS

// ui holds the parsed templates and the static file handler.
type ui struct {
	pages  map[string]*template.Template
	static http.Handler
}

// pageNames are the templates that extend base.html. Listed explicitly rather
// than globbed, so a stray file cannot become a page and a missing one fails at
// startup instead of on the request that needed it.
var pageNames = []string{"login", "index"}

// newUI parses every page template.
//
// Each page is its own template set — base plus that page — because html/template
// resolves blocks per set, and parsing every page into one set would leave the
// last-parsed definition of "content" winning for all of them.
func newUI() (*ui, error) {
	u := &ui{pages: make(map[string]*template.Template, len(pageNames))}

	for _, name := range pageNames {
		t, err := template.New("base.html").ParseFS(assets,
			"templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parsing the %s template: %w", name, err)
		}
		u.pages[name] = t
	}

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
	// (§5.4), but a second, independent restraint costs one header: no inline
	// script, no third-party anything, and images only from this origin — which is
	// exactly what a localized archive needs and nothing more.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)

	if _, err := buf.WriteTo(w); err != nil {
		// The client hung up mid-write. Nothing to do but say so at a level that
		// does not page anyone.
		s.log.Debug("writing the response failed", "page", page, "error", err)
	}
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
