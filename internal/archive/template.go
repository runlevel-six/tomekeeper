package archive

import "html/template"

// articleTemplate renders a standalone article page.
//
// Everything is inline and nothing is fetched. No stylesheet link, no font
// import, no script — because the page must render correctly with this service
// stopped, the database gone, and the machine offline, opened straight from a
// file manager in ten years. Every external reference is a dependency on
// something outliving the archive, and the whole point is that nothing has to.
//
// The CSS is deliberately small and unopinionated. It is a readable column with
// sensible defaults, not a design; a design would date badly and this file is
// meant to still be pleasant in 2036.
var articleTemplate = template.Must(template.New("article").Parse(`<!DOCTYPE html>
<html lang="{{if .Language}}{{.Language}}{{else}}en{{end}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
{{if .Author}}<meta name="author" content="{{.Author}}">{{end}}
<style>
:root {
  color-scheme: light dark;
  --bg: #fdfdfc; --fg: #1a1a19; --muted: #5c5c58; --rule: #e0e0dc; --link: #1a4d8f;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #16161a; --fg: #e6e6e3; --muted: #a0a09a; --rule: #2f2f36; --link: #7ab3f0; }
}
body {
  background: var(--bg); color: var(--fg);
  font: 1.05rem/1.65 Georgia, 'Iowan Old Style', 'Times New Roman', serif;
  margin: 0 auto; padding: 2.5rem 1.25rem 6rem; max-width: 38rem;
  overflow-wrap: break-word;
}
header { border-bottom: 1px solid var(--rule); margin-bottom: 2rem; padding-bottom: 1.25rem; }
h1 { font-size: 1.9rem; line-height: 1.2; margin: 0 0 .6rem; }
h2, h3, h4 { line-height: 1.25; margin-top: 2rem; }
.meta { color: var(--muted); font-size: .875rem; margin: 0; }
.meta a { color: inherit; }
a { color: var(--link); }
img, picture, video, svg { max-width: 100%; height: auto; display: block; margin: 1.5rem auto; }
figure { margin: 1.75rem 0; }
figcaption { color: var(--muted); font-size: .85rem; text-align: center; margin-top: .5rem; }
blockquote {
  border-left: 3px solid var(--rule); margin: 1.5rem 0; padding: 0 0 0 1.25rem; color: var(--muted);
}
pre {
  background: rgba(127,127,127,.12); padding: 1rem; overflow-x: auto;
  font-size: .85rem; line-height: 1.5; border-radius: 3px;
}
code { font-family: ui-monospace, 'SF Mono', Menlo, Consolas, monospace; font-size: .88em; }
pre code { font-size: inherit; }
table { border-collapse: collapse; width: 100%; font-size: .92rem; }
th, td { border: 1px solid var(--rule); padding: .4rem .6rem; text-align: left; }
hr { border: 0; border-top: 1px solid var(--rule); margin: 2.5rem 0; }
footer {
  border-top: 1px solid var(--rule); color: var(--muted);
  font-size: .8rem; margin-top: 3.5rem; padding-top: 1.25rem;
}
</style>
</head>
<body>
<header>
<h1>{{.Title}}</h1>
<p class="meta">
{{- if .Author}}{{.Author}}{{end -}}
{{- if and .Author .SiteName}} · {{end -}}
{{- if .SiteName}}{{.SiteName}}{{end -}}
{{- if .PublishedDate}}{{if or .Author .SiteName}} · {{end}}{{.PublishedDate}}{{end -}}
</p>
<p class="meta"><a href="{{.URL}}" rel="noreferrer">{{.URL}}</a></p>
</header>

<main>
{{.ContentHTML}}
</main>

<footer>
<p>Archived by tomekeeper on {{.ArchivedDate}}{{if .WordCount}} · {{.WordCount}} words{{end}}.</p>
<p>The original page is stored beside this file as <code>raw.html.gz</code>; the
record of this article, including its images, is in <code>meta.json</code>.</p>
</footer>
</body>
</html>
`))
