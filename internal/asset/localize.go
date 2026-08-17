package asset

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// LocalPrefix is how a localized image is referenced in the stored body.
//
// Root-relative, because that is what the web UI can route. The archive writer
// converts these to file-relative paths for index.html, which is opened from
// disk where there is no root.
const LocalPrefix = "/"

// Resolver localizes one image.
//
// It is given the absolute source URL and returns the store-relative path the
// image now lives at, or ok=false if it could not be localized. A resolver
// does the fetching, processing, and storing; Localize only rewrites markup.
type Resolver func(sourceURL string) (storePath string, ok bool)

// Outcome summarizes what Localize did.
type Outcome struct {
	// Found is how many image references the body contained.
	Found int

	// Localized is how many now point into the archive.
	Localized int

	// Failed is how many kept their original absolute URL.
	//
	// These are not errors that stop anything: a hotlinked image that
	// still loads is better than a broken one, and the article is marked
	// 'partial' so the gap is visible rather than silent.
	Failed int
}

// Localize flattens responsive image markup and rewrites what it can into the
// archive.
//
// Two things happen in one DOM pass, because they are the same decision. A
// <picture> element or a srcset offers the same image at several sizes; the
// archive keeps exactly one, so the element is flattened to a plain <img>
// pointing at the candidate nearest MaxDimension. Archiving all of them
// would multiply the storage cost of every illustrated article for a benefit
// the archive cannot use, since it renders at one size.
func Localize(body string, resolve Resolver) (string, Outcome) {
	var outcome Outcome

	if strings.TrimSpace(body) == "" {
		return body, outcome
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return body, outcome
	}

	flattenPictures(doc)

	doc.Find("img").Each(func(_ int, img *goquery.Selection) {
		src := chooseSource(img)
		if src == "" {
			return
		}
		outcome.Found++

		// Whatever happens below, the responsive attributes go: the archive
		// holds one size, so leaving a srcset would point a reader's browser
		// back at the origin for a picture that is already stored here.
		img.RemoveAttr("srcset")
		img.RemoveAttr("sizes")

		if ok, _ := ShouldFetch(src); !ok {
			// A data URI is already self-contained, and anything else
			// unfetchable is left exactly as it is.
			img.SetAttr("src", src)
			return
		}

		storePath, ok := resolve(src)
		if !ok {
			img.SetAttr("src", src)
			outcome.Failed++
			return
		}

		img.SetAttr("src", LocalPrefix+strings.TrimPrefix(storePath, "/"))
		outcome.Localized++
	})

	out, err := doc.Find("body").Html()
	if err != nil {
		return body, outcome
	}
	return out, outcome
}

// flattenPictures replaces each <picture> with the <img> inside it.
//
// A <picture> exists to let a browser choose between formats and sizes at
// render time. The archive has already made that choice — one file, one
// format — so the element has nothing left to do, and keeping it would leave
// <source> elements pointing at the origin.
func flattenPictures(doc *goquery.Document) {
	doc.Find("picture").Each(func(_ int, picture *goquery.Selection) {
		img := picture.Find("img").First()
		if img.Length() == 0 {
			// A <picture> with only <source> children: promote the best
			// candidate to a real image rather than losing it.
			if src := bestSourceIn(picture); src != "" {
				picture.ReplaceWithHtml(`<img src="` + escapeAttr(src) + `">`)
				return
			}
			picture.Remove()
			return
		}

		// The <img> inside a <picture> is the fallback and often has the
		// smallest source, so a better candidate from the <source> elements
		// is preferred when one exists.
		if _, hasSrcset := img.Attr("srcset"); !hasSrcset {
			if src := bestSourceIn(picture); src != "" {
				img.SetAttr("src", src)
			}
		}

		html, err := goquery.OuterHtml(img)
		if err != nil {
			return
		}
		picture.ReplaceWithHtml(html)
	})
}

// bestSourceIn picks the candidate nearest MaxDimension across a picture's
// <source> elements.
func bestSourceIn(picture *goquery.Selection) string {
	var best string

	picture.Find("source").EachWithBreak(func(_ int, source *goquery.Selection) bool {
		if srcset, ok := source.Attr("srcset"); ok {
			if candidate := SelectFromSrcset(srcset); candidate != "" {
				best = candidate
				return false // the first <source> is the highest priority
			}
		}
		if src, ok := source.Attr("src"); ok && src != "" {
			best = src
			return false
		}
		return true
	})
	return best
}

// chooseSource returns the URL to archive for an <img>, preferring the srcset
// candidate nearest the target size over the plain src.
func chooseSource(img *goquery.Selection) string {
	if srcset, ok := img.Attr("srcset"); ok {
		if candidate := strings.TrimSpace(SelectFromSrcset(srcset)); candidate != "" {
			return candidate
		}
	}

	src, _ := img.Attr("src")
	return strings.TrimSpace(src)
}

// escapeAttr escapes a URL for insertion into an attribute.
func escapeAttr(s string) string {
	return strings.NewReplacer(`"`, "&quot;", "<", "&lt;", ">", "&gt;", "&", "&amp;").Replace(s)
}
