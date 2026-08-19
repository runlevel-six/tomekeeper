package extract

import (
	"bytes"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// minSlugLength is the shortest article slug worth matching image URLs against.
//
// A two-character slug would match half the images on a page by coincidence, and
// the precision of this whole rung rests on the match meaning something.
const minSlugLength = 4

// maxPageImages caps how many images one page may contribute.
//
// Long-form strips run to twenty panels or more, so this is generous; the slug
// match is what does the real constraining. It exists only so that a page whose
// slug happens to be a common word cannot pull down a whole gallery.
const maxPageImages = 24

// viaPageImages builds a body from the page's own images, for articles whose
// content is a picture rather than prose.
//
// This is the rung for webcomics, and it exists because none of the others can
// ever work for them. Every text-based extractor scores by text density, and a
// comic has almost none — so trafilatura and readability do not merely rank the
// comic poorly, they cannot see it at all, and settle on the largest block of
// words on the page instead. On one strip that was the news sidebar: seventy-eight
// words of shop announcements, stored as the article, with the comic absent.
//
// The acceptance floor then closes the last door. minChars rejects anything under
// two hundred characters as a paywall stub or a navigation shell, which is right
// for text and exactly wrong here: a comic is one image and a caption, so even a
// hand-written domain rule pointing straight at it produced nothing. That is why
// this rung does not consult the floor.
//
// The problem it has to solve is telling the comic from the furniture, on pages
// carrying twenty images of navigation arrows, logos and banners. The first signal
// is that a content image's URL contains the article's own slug, while chrome lives
// under generic paths shared by every page on the site:
//
//	/comics/oots1347.html   → /comics/oots/oots1347_a1b2….png    (chrome: /redesign/ComicNav_Next.gif)
//	/2016/project-lifecycle → /2016/project-lifecycle/4-….png    (chrome: /images/logo.png)
//	/comics/design_hell     → /comics/design_hell/1.png, 2.jpg   (chrome: /default/header_2023/….png)
//
// The second is that the image file is *named after the strip*, which is what
// rescues a site whose article URLs are bare numbers and whose images therefore
// share nothing with them:
//
//	/3286 ("Particle Physics Equipment") → /comics/particle_physics_equipment.png
//
// It is a precise signal rather than a clever one, which is the point: a false
// positive stores a banner in place of an article, so this would rather find
// nothing and fall through than guess. Sites whose images are named after neither
// are not rescued here and still need a domain rule.
func (e *Extractor) viaPageImages(in Input, pageURL *url.URL) (Result, bool) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(in.RawHTML))
	if err != nil {
		return Result{}, false
	}

	meta := docMetadata(doc)

	slug := articleSlug(pageURL)
	if len(slug) < minSlugLength {
		slug = ""
	}
	titles := titleSlugs(meta.Title)
	if slug == "" && len(titles) == 0 {
		return Result{}, false
	}

	var (
		body    strings.Builder
		caption []string
		seen    = make(map[string]bool)
		found   int
	)

	doc.Find("img").EachWithBreak(func(_ int, img *goquery.Selection) bool {
		if found >= maxPageImages {
			return false
		}

		src, ok := img.Attr("src")
		if !ok {
			// A lazy-loading page keeps the real URL out of src until scrolled.
			if src, ok = img.Attr("data-src"); !ok {
				return true
			}
		}
		src = strings.TrimSpace(src)
		if src == "" || seen[src] {
			return true
		}

		resolved := src
		if u, err := pageURL.Parse(src); err == nil {
			resolved = u.String()
		}
		if !namesTheArticle(resolved, slug, titles) {
			return true
		}

		seen[src] = true
		found++

		// Both the alt text and the hover text, because on a comic they are two
		// different things and the second is often the joke. An xkcd strip puts the
		// strip's name in alt and the punchline in title, so keeping only one of
		// them archives half the comic — and only the half that is already the
		// article's title.
		alt := strings.TrimSpace(img.AttrOr("alt", ""))
		hover := strings.TrimSpace(img.AttrOr("title", ""))
		if alt == "" {
			alt, hover = hover, ""
		}

		body.WriteString(`<figure><img src="`)
		body.WriteString(escapeAttr(resolved))
		body.WriteString(`"`)
		if alt != "" {
			body.WriteString(` alt="` + escapeAttr(alt) + `"`)
			caption = append(caption, alt)
		}
		body.WriteString(`>`)
		// The hover text becomes a visible caption rather than a title attribute.
		// Two reasons, and the second is the one that decided it: an archive read
		// years later should not hide a punchline behind a mouse, and the sanitizer's
		// allowlist matches a title attribute against a pattern that rejects
		// quotation marks and question marks — so `"Want to feel old?" "Yes."` would
		// survive on one strip and vanish on the next. A figcaption is text, and text
		// is kept whatever it says.
		if hover != "" && hover != alt {
			body.WriteString(`<figcaption>` + escapeAttr(hover) + `</figcaption>`)
			caption = append(caption, hover)
		}
		body.WriteString(`</figure>`)
		return true
	})

	if found == 0 {
		return Result{}, false
	}

	// The text is whatever the images called themselves — their alt and hover text,
	// which is usually the strip's name and its punchline, and sometimes nothing at
	// all. It is what search indexes, so a comic is findable by its joke rather than
	// by nothing. Deliberately not padded with the page's other text: the surrounding
	// words are the navigation this rung exists to avoid, and a body that quietly
	// included them would be the original bug wearing a different name.
	text := strings.TrimSpace(strings.Join(caption, " "))

	return e.finish(NamePageImages, body.String(), text, pageURL, meta), true
}

// namesTheArticle reports whether an image is this article's content rather than
// the furniture around it.
//
// Either signal is enough. The slug match is a substring, because a site appends a
// hash or a panel number to the name it derived from the URL. The title match is
// exact, because the title is not part of the address and a substring of it would
// be a coincidence: a page called "Fifteen Years" is not evidence about an image
// called `fifteen.png`.
func namesTheArticle(resolved, slug string, titles []string) bool {
	if slug != "" && strings.Contains(strings.ToLower(resolved), slug) {
		return true
	}

	name := imageSlug(resolved)
	if name == "" {
		return false
	}
	for _, title := range titles {
		if name == title {
			return true
		}
	}
	return false
}

// titleSlugs are the file names this page's own title could have produced.
//
// Two of them: the whole title, and — when the title carries a separator — the part
// after the last one. Sites prefix the site's name onto every title (`xkcd:
// Particle Physics Equipment`) while naming the file after the article alone, and
// the prefix is the half that is the same on every page.
//
// Only the last segment, deliberately, and never the first: the first is the site's
// name, and accepting it would match the site's logo on every page it appears on.
func titleSlugs(title string) []string {
	var out []string
	add := func(s string) {
		s = slugify(s)
		if len(s) < minSlugLength {
			return
		}
		for _, have := range out {
			if have == s {
				return
			}
		}
		out = append(out, s)
	}

	add(title)
	if segments := strings.FieldsFunc(title, isTitleSeparator); len(segments) > 1 {
		add(segments[len(segments)-1])
	}
	return out
}

// isTitleSeparator reports whether a rune is one of the characters sites use to
// join a site's name to an article's.
func isTitleSeparator(r rune) bool { return strings.ContainsRune(":|·—–", r) }

// imageSlug is an image's file name without its extension, normalized the way a
// title is, so that `particle_physics_equipment.png` and "Particle Physics
// Equipment" can be compared.
func imageSlug(resolved string) string {
	name := resolved
	if u, err := url.Parse(resolved); err == nil {
		name = u.Path
	}
	name = path.Base(name)
	return slugify(strings.TrimSuffix(name, path.Ext(name)))
}

// slugify reduces a string to lowercase words joined by single hyphens, so that
// the same name written for a file and written for a reader compare equal.
//
// Letters and digits in any script, not just ASCII: a title in Cyrillic and a file
// named in Cyrillic are the same match as an English pair, because url.Parse hands
// back a decoded path.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

// articleSlug is the distinctive part of an article's URL, lowercased.
//
// The last path segment without its extension: "oots1347" from
// /comics/oots1347.html, "project-lifecycle" from /2016/project-lifecycle.
func articleSlug(pageURL *url.URL) string {
	segment := path.Base(strings.TrimSuffix(pageURL.Path, "/"))
	if segment == "." || segment == "/" {
		return ""
	}
	segment = strings.TrimSuffix(segment, path.Ext(segment))
	return strings.ToLower(strings.TrimSpace(segment))
}

// escapeAttr escapes a string for use in a double-quoted HTML attribute, and — its
// replacements being a superset of what text needs — as element text.
//
// The output goes through the sanitizer afterwards, so this is belt and braces
// rather than the only defense — but building markup by concatenation without
// escaping is how the second line of defense becomes the only one.
func escapeAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"'", "&#39;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// hasImage reports whether a body contains an image element.
func hasImage(html string) bool {
	return strings.Contains(strings.ToLower(html), "<img")
}

// imageTextCeiling is the longest text a body may carry and still be treated as
// having missed an image-first page.
//
// Generous enough to cover the observed failure — a comic page whose sidebar of
// shop announcements extracted as seventy-eight words of "article" — and short
// enough that no real piece of writing falls inside it.
const imageTextCeiling = 120

// orThePageImagesIfTextless replaces a thin body that missed the picture with the
// page's own images.
//
// The counterpart to orTheFeedIfRicher, for pages with no feed body to fall back
// on. A comic page is not empty of text — it has navigation, an archive list,
// shop announcements — so a text extractor does not fail on it. It succeeds, on
// the wrong element, and the ladder stops before ever reaching the images.
//
// Two conditions, both required, because replacing an extracted body is
// destructive: the body must be too short to be a real piece of writing, and the
// page must carry images that name the article which the body does not already
// contain. A body holding the article's own picture is left alone whatever its
// length — extraction found it, and this rung has nothing to add.
//
// "The body already has the picture" and "the body has an image" are not the same
// test, which is what this used to get wrong. A comic page's footer carries images
// too — a sidebar thumbnail, a banner — so an extraction that returned the footer
// and none of the comic satisfied a check for any image at all and was left in
// place. Every xkcd strip in a real archive was stored that way: 83 words of
// "Comics I enjoy" and two pictures, neither of them the strip. The images this
// rung finds are the ones it can name; whether the body holds one of *those* is the
// question worth asking.
func (e *Extractor) orThePageImagesIfTextless(in Input, pageURL *url.URL, page Result) Result {
	if page.WordCount > imageTextCeiling {
		return page
	}
	images, ok := e.viaPageImages(in, pageURL)
	if !ok {
		return page
	}
	if sharesAnImage(page.HTML, images.HTML) {
		return page
	}
	return images
}

// sharesAnImage reports whether two bodies have an image source in common.
func sharesAnImage(body, other string) bool {
	have := imageSources(body)
	if len(have) == 0 {
		return false
	}
	for src := range imageSources(other) {
		if have[src] {
			return true
		}
	}
	return false
}

// imageSources is the set of image sources a body references.
func imageSources(body string) map[string]bool {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil
	}

	out := make(map[string]bool)
	doc.Find("img").Each(func(_ int, img *goquery.Selection) {
		if src := strings.TrimSpace(img.AttrOr("src", "")); src != "" {
			out[src] = true
		}
	})
	return out
}
