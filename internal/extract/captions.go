package extract

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// markCaptions turns an image's caption into a figcaption, while the page still says
// which elements are captions. The domain-rule rung is its only caller; see there for
// why.
//
// The reader styles a figcaption as a caption: smaller, softer, set apart from the
// prose. Many sites never use the element. They mark a caption with a class — WordPress
// writes `wp-caption-text`, a lightbox library writes `pswp-caption-content` — and the
// sanitizer drops every class, so by the time a body is stored the caption is a bare div
// and reads as another paragraph of the article. This is the last point at which the
// difference can be seen.
//
// An element is taken as a caption on either of two signals:
//
//   - An image names it with aria-labelledby or aria-describedby. That is the site
//     saying, in the accessibility tree, that this text describes that picture.
//   - A class token is exactly "caption", or has "caption" as one of its hyphen- or
//     underscore-separated parts, and an image sits close by.
//
// Either way it must hold no media itself — a wrapper around the picture *and* its
// caption is not the caption — and must not already be inside one. Only the element's
// name changes; its contents, and every attribute, stay as they were.
func markCaptions(root *html.Node) {
	labeled := labeledByImages(root)
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data == "figcaption" {
				continue
			}
			if captionCandidates[c.Data] && !holdsPicture(c) && !insideParagraph(c) &&
				(labeled[attr(c, "id")] || (classSaysCaption(attr(c, "class")) && nearImage(c))) {
				c.Data, c.DataAtom = "figcaption", atom.Figcaption
				continue
			}
			walk(c)
		}
	}
	walk(root)
}

// captionCandidates are the elements a caption is written as. Headings and list items
// are left alone: a class naming them a caption is a site's styling, and renaming one
// would take it out of the document's structure.
var captionCandidates = map[string]bool{"div": true, "p": true, "span": true}

// labeledByImages returns the ids an image names as its label or description.
func labeledByImages(root *html.Node) map[string]bool {
	ids := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, key := range []string{"aria-labelledby", "aria-describedby"} {
				for _, id := range strings.Fields(attr(n, key)) {
					ids[id] = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	delete(ids, "")
	return ids
}

// classSaysCaption reports whether a class attribute names its element a caption.
//
// Whole parts only: "caption-content" and "wp-caption-text" are captions, "captioned"
// and "recaptcha" are not.
func classSaysCaption(class string) bool {
	for _, token := range strings.Fields(strings.ToLower(class)) {
		for _, part := range strings.FieldsFunc(token, func(r rune) bool { return r == '-' || r == '_' }) {
			if part == "caption" {
				return true
			}
		}
	}
	return false
}

// nearImage reports whether an image sits within a few levels of an element: inside the
// same small container, as a caption does, rather than somewhere else on the page.
func nearImage(n *html.Node) bool {
	const levels = 3
	ancestor := n
	for i := 0; i < levels && ancestor.Parent != nil; i++ {
		ancestor = ancestor.Parent
		if ancestor.Type == html.ElementNode && (ancestor.Data == "body" || ancestor.Data == "article" || ancestor.Data == "main") {
			return false
		}
		if holdsPicture(ancestor) {
			return true
		}
	}
	return false
}

// pictureElements are what a caption describes. Narrower than mediaElements on purpose:
// an svg is as often a caption's own icon as a picture, and counting it would stop the
// caption that carries one from ever being recognized.
var pictureElements = map[string]bool{"img": true, "picture": true, "video": true, "iframe": true}

func holdsPicture(n *html.Node) bool {
	if n.Type == html.ElementNode && pictureElements[n.Data] {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if holdsPicture(c) {
			return true
		}
	}
	return false
}

// insideParagraph reports whether an element sits in running text. A figcaption there
// would end the paragraph where it stands, so a caption-classed span inside a <p> stays
// a span.
func insideParagraph(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "p" {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
