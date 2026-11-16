package extract

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// pruneUnseen removes from a parsed page what a reader of that page never sees, before
// any rung looks at it.
//
// Every rung measures text, and text a site hides is still text. Two real shapes made
// this necessary, both of which stored the same words over and over:
//
//   - An author-box plugin that ships, inside `style="display:none"`, a list of every
//     post the author has ever written. On one news site that was a thousand entries;
//     the extractor kept the author's name from each, and the article ended with the
//     byline a thousand times. A third of that site's archive carried it.
//   - A lightbox that keeps a second copy of each figure's caption beside the image,
//     for the zoomed view, and hides it with the site's stylesheet. Every caption in
//     the article appeared twice.
//
// The first is handled by removing hidden elements, the second by removing a figure's
// copy of its own caption. Both act on elements and never on attributes, so an image's
// alt text and its hover text survive whenever the image does.
func pruneUnseen(root *html.Node) {
	removeHidden(root)
	removeCaptionCopies(root)
}

// neverPruned are elements whose being hidden says nothing about their contents.
//
// Anti-flicker snippets for A/B testing hide the whole body inline and reveal it from
// script, and a stored page is read without running that script. Taking the body as
// hidden would discard every article on such a site.
var neverPruned = map[string]bool{"html": true, "head": true, "body": true}

// removeHidden drops elements hidden by an inline style of `display: none` or
// `visibility: hidden`.
//
// Inline only. A stylesheet can hide anything, and resolving one is a browser's job, so
// an element hidden by a class is not caught here — that is what a domain rule's strip
// selectors are for.
//
// Deliberately not the `hidden` attribute, although it means the same thing to a
// browser. Streaming server rendering sends the finished article inside
// `<div hidden id="S:0">` and swaps it into place from script, which a stored page never
// runs; treating the attribute as hidden emptied every post on one such site, a dozen of
// them over 1,500 words.
func removeHidden(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && !neverPruned[c.Data] && isHidden(c) {
			n.RemoveChild(c)
		} else {
			removeHidden(c)
		}
		c = next
	}
}

func isHidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, "style") && styleHides(a.Val) {
			return true
		}
	}
	return false
}

// styleHides reports whether an inline style declaration hides its element.
func styleHides(style string) bool {
	for _, decl := range strings.Split(style, ";") {
		prop, value, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimSpace(strings.TrimSuffix(value, "!important"))
		if (prop == "display" && value == "none") || (prop == "visibility" && value == "hidden") {
			return true
		}
	}
	return false
}

// removeCaptionCopies drops, inside each figure, any element that repeats the figure's
// caption word for word.
//
// The figcaption is the copy kept, because it is the one the markup says is the
// caption. Elements carrying media are never removed, whatever their text, so this
// cannot take an image with it. The comparison ignores whitespace: the two copies are
// usually laid out differently, and a block edge in one is a space in the other.
func removeCaptionCopies(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "figure" {
			if caption := figcaptionOf(c); caption != nil {
				if want := squashedText(caption); want != "" {
					removeCopies(c, caption, want)
				}
			}
		}
		removeCaptionCopies(c)
	}
}

// figcaptionOf returns a figure's own caption, not one belonging to a figure nested in
// it.
func figcaptionOf(figure *html.Node) *html.Node {
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || c.Data == "figure" {
				continue
			}
			if c.Data == "figcaption" {
				return c
			}
			if found := find(c); found != nil {
				return found
			}
		}
		return nil
	}
	return find(figure)
}

func removeCopies(n, caption *html.Node, want string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && c != caption && c.Data != "figure" {
			if !holdsMedia(c) && squashedText(c) == want {
				n.RemoveChild(c)
			} else {
				removeCopies(c, caption, want)
			}
		}
		c = next
	}
}

// mediaElements are what makes an element more than its text.
var mediaElements = map[string]bool{
	"img": true, "picture": true, "video": true, "audio": true,
	"svg": true, "iframe": true, "object": true, "embed": true, "canvas": true,
}

func holdsMedia(n *html.Node) bool {
	if n.Type == html.ElementNode && mediaElements[n.Data] {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if holdsMedia(c) {
			return true
		}
	}
	return false
}

// squashedText is an element's text with every space removed, for comparing two
// copies of the same words laid out differently.
func squashedText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			for _, r := range n.Data {
				if !unicode.IsSpace(r) {
					b.WriteRune(r)
				}
			}
		case n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style"):
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
