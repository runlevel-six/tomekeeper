package exchange

import (
	"regexp"
	"strings"
)

// imgSrc finds the src of every img tag in a body.
//
// A regular expression rather than a parse, and only here: this is counting for a
// report, not rewriting markup. Nothing acts on the result except a number a
// person reads, so an occasional miscount in exotic markup costs nothing — whereas
// parsing every body twice, once to count and once to store, costs real time on a
// library of thousands.
var imgSrc = regexp.MustCompile(`(?i)<img[^>]+src\s*=\s*["']([^"']+)["']`)

// otherScheme matches a reference carrying any scheme, so that one which is not
// http or https can be told from a relative path.
var otherScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// sourceStoragePath is what an image localized by the *source* system looks like.
//
// Wallabag rewrites image references to its own storage when `download_images` is
// enabled, and this is the shape of that path. Detecting it matters because those
// images live inside the other installation: this archive cannot fetch them, and an
// import from an instance with the setting on carries bodies whose pictures are
// only reachable from the machine being migrated away from. That is worth saying
// out loud in a report rather than discovering later as an article of blank frames.
const sourceStoragePath = "/assets/images/"

// ImageCensus is what an import can say about the images in a library.
type ImageCensus struct {
	// Fetchable point at the site the article came from, which is where this
	// archive can go and get them. On an export from an instance with image
	// downloading off — the common case, and the maintainer's own — this is nearly
	// all of them.
	Fetchable int

	// InSourceStorage point inside the exporting installation. Unreachable from
	// here, so they are the one image failure an import can predict rather than
	// discover.
	InSourceStorage int

	// SelfContained are data: URIs, which need no fetch and are left alone.
	SelfContained int

	// Unusable carry a scheme this archive will not store — a `denied:data:…`
	// placeholder written by whatever blocked the original, most often. The
	// sanitizer drops them on the way in, so counting them separately keeps them out
	// of the number of images an import claims it is about to archive.
	Unusable int
}

// Total is every image reference found.
func (c ImageCensus) Total() int {
	return c.Fetchable + c.InSourceStorage + c.SelfContained + c.Unusable
}

// Add accumulates another body's census.
func (c *ImageCensus) Add(other ImageCensus) {
	c.Fetchable += other.Fetchable
	c.InSourceStorage += other.InSourceStorage
	c.SelfContained += other.SelfContained
	c.Unusable += other.Unusable
}

// isSourceStorage reports whether a reference points into the exporting
// installation's own image storage.
//
// The reference must be *relative* as well as carrying the storage path, and that
// second condition was learned from real data rather than reasoned out. Wallabag
// rewrites a localized image to a path relative to its own root, so a relative
// reference is the signal. An absolute one belongs to the site the article came
// from — and `/assets/images/` is an ordinary layout for a static site generator,
// so matching on the path alone reported one of the maintainer's own articles as
// having its picture stranded in an installation it had never been near.
func isSourceStorage(src string) bool {
	if otherScheme.MatchString(src) || strings.HasPrefix(src, "//") {
		return false
	}
	return strings.Contains(src, sourceStoragePath)
}

// censusOf classifies the image references in one body.
func censusOf(body string) ImageCensus {
	var c ImageCensus
	if body == "" {
		return c
	}

	for _, match := range imgSrc.FindAllStringSubmatch(body, -1) {
		src := strings.TrimSpace(match[1])
		switch {
		case src == "":
		case strings.HasPrefix(src, "data:"):
			c.SelfContained++
		case isSourceStorage(src):
			c.InSourceStorage++
		case strings.HasPrefix(src, "http://"),
			strings.HasPrefix(src, "https://"),
			strings.HasPrefix(src, "//"):
			c.Fetchable++
		case otherScheme.MatchString(src):
			// A scheme that is neither http nor https. The sanitizer will not keep
			// it, so it is not an image this archive is going to hold.
			c.Unusable++
		default:
			// Relative, and therefore fetchable: the body is resolved against the
			// article's own URL before it is stored, which turns these into absolute
			// references to the site the article came from.
			c.Fetchable++
		}
	}
	return c
}
