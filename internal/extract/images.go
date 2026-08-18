package extract

import (
	"bytes"
	"net/url"
	"path"
	"strings"

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
// carrying twenty images of navigation arrows, logos and banners. The signal used
// here is that a content image's URL contains the article's own slug, while
// chrome lives under generic paths shared by every page on the site:
//
//	/comics/oots1347.html   → /comics/oots/oots1347_a1b2….png    (chrome: /redesign/ComicNav_Next.gif)
//	/2016/project-lifecycle → /2016/project-lifecycle/4-….png    (chrome: /images/logo.png)
//	/comics/design_hell     → /comics/design_hell/1.png, 2.jpg   (chrome: /default/header_2023/….png)
//
// It is a precise signal rather than a clever one, which is the point: a false
// positive stores a banner in place of an article, so this would rather find
// nothing and fall through than guess. Sites whose image URLs share nothing with
// their page URLs are not rescued here and still need a domain rule.
func (e *Extractor) viaPageImages(in Input, pageURL *url.URL) (Result, bool) {
	slug := articleSlug(pageURL)
	if len(slug) < minSlugLength {
		return Result{}, false
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(in.RawHTML))
	if err != nil {
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
		if !strings.Contains(strings.ToLower(resolved), slug) {
			return true
		}

		seen[src] = true
		found++

		alt := strings.TrimSpace(img.AttrOr("alt", ""))
		if alt == "" {
			alt = strings.TrimSpace(img.AttrOr("title", ""))
		}

		body.WriteString(`<p><img src="`)
		body.WriteString(escapeAttr(resolved))
		body.WriteString(`"`)
		if alt != "" {
			body.WriteString(` alt="` + escapeAttr(alt) + `"`)
			caption = append(caption, alt)
		}
		body.WriteString(`></p>`)
		return true
	})

	if found == 0 {
		return Result{}, false
	}

	// The text is whatever the images called themselves, which is usually the
	// strip's title and sometimes nothing at all. Deliberately not padded with
	// the page's other text: the surrounding words are the navigation this rung
	// exists to avoid, and a body that quietly included them would be the
	// original bug wearing a different name.
	text := strings.TrimSpace(strings.Join(caption, " "))

	return e.finish(NamePageImages, body.String(), text, pageURL, docMetadata(doc)), true
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

// escapeAttr escapes a string for use in a double-quoted HTML attribute.
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

// orThePageImagesIfTextless replaces a thin, imageless body with the page's own
// images when the page clearly has some.
//
// The counterpart to orTheFeedIfRicher, for pages with no feed body to fall back
// on. A comic page is not empty of text — it has navigation, an archive list,
// shop announcements — so a text extractor does not fail on it. It succeeds, on
// the wrong element, and the ladder stops before ever reaching the images.
//
// Three conditions, all required, because replacing an extracted body is
// destructive: the body must carry no image at all, it must be too short to be
// a real piece of writing, and the page must have images bearing the article's
// own slug. A body that already has an image is left alone whatever its length —
// extraction found the picture, and this rung has nothing to add.
func (e *Extractor) orThePageImagesIfTextless(in Input, pageURL *url.URL, page Result) Result {
	if hasImage(page.HTML) || page.WordCount > imageTextCeiling {
		return page
	}
	if images, ok := e.viaPageImages(in, pageURL); ok {
		return images
	}
	return page
}
