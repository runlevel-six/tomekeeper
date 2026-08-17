// Package urlcanon normalizes URLs so that the same article reached by
// different routes produces the same key.
//
// Deduplication depends entirely on this function, and deduplication is what
// makes an article — rather than a feed item — the root entity of the archive.
// Get it wrong in the permissive direction and two feeds carrying the same
// story become two archived copies with two sets of images. Get it wrong in
// the aggressive direction and two genuinely different articles collapse into
// one, which loses data permanently.
//
// Because of that asymmetry, the rules here strip only parameters that are
// known to be tracking noise. Anything unrecognized is preserved: plenty of
// content management systems address articles with query parameters, and
// discarding `?p=123` would merge every article on the site.
//
// The function is pure and golden-file tested against testdata/urls/.
package urlcanon

import (
	"fmt"
	"net/url"
	"strings"
)

// trackingParams are query parameters removed unconditionally. Every entry is
// a parameter that identifies the *route taken to the article* rather than the
// article, so removing it cannot change which document is addressed.
var trackingParams = map[string]bool{
	"utm_source":           true,
	"utm_medium":           true,
	"utm_campaign":         true,
	"utm_term":             true,
	"utm_content":          true,
	"utm_name":             true,
	"utm_reader":           true,
	"utm_brand":            true,
	"utm_social":           true,
	"utm_social_type":      true,
	"fbclid":               true,
	"gclid":                true,
	"dclid":                true,
	"msclkid":              true,
	"yclid":                true,
	"igshid":               true,
	"mc_cid":               true,
	"mc_eid":               true,
	"ref":                  true,
	"source":               true,
	"__twitter_impression": true,
	"_hsenc":               true,
	"_hsmi":                true,
	"vero_conv":            true,
	"vero_id":              true,
	"wt_zmc":               true,
	"at_medium":            true,
	"at_campaign":          true,
}

// utmPrefix catches the long tail of vendor-specific utm_* parameters that are
// not worth enumerating individually.
const utmPrefix = "utm_"

// defaultPorts are stripped when they match the scheme, because
// https://example.com:443/x and https://example.com/x are the same resource
// and servers will not distinguish them.
var defaultPorts = map[string]string{
	"http":  "80",
	"https": "443",
}

// Canonicalize returns the canonical form of rawURL.
//
// It returns an error for input that is not an absolute http or https URL.
// Callers should treat that as "this reference is unusable" rather than
// falling back to the raw string, because a non-canonical key in the articles
// table defeats the deduplication that every later milestone assumes.
func Canonicalize(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty URL")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parsing URL %q: %w", rawURL, err)
	}

	// Scheme: lowercase, and only the two we archive.
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "http", "https":
	case "":
		return "", fmt.Errorf("URL %q has no scheme; it must be absolute", rawURL)
	default:
		return "", fmt.Errorf("URL %q has unsupported scheme %q", rawURL, u.Scheme)
	}

	if u.Host == "" {
		return "", fmt.Errorf("URL %q has no host", rawURL)
	}

	if err := canonicalizeHost(u); err != nil {
		return "", fmt.Errorf("URL %q: %w", rawURL, err)
	}
	canonicalizePath(u)
	canonicalizeQuery(u)
	canonicalizeFragment(u)

	// Credentials in a URL are not part of the document's identity, and
	// storing them would put passwords in a column that is displayed in the
	// UI and included in exports.
	u.User = nil

	return u.String(), nil
}

// canonicalizeHost lowercases the host and strips the port when it is the
// scheme's default.
//
// The host is case-insensitive by definition (RFC 3986), unlike the path.
func canonicalizeHost(u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	port := u.Port()

	// A trailing dot denotes the DNS root and addresses the same server.
	// TrimRight rather than TrimSuffix so that the result is a fixed point.
	host = strings.TrimRight(host, ".")

	// Found by FuzzCanonicalize: "http://." has a non-empty host that
	// normalizes to nothing. Emitting it would produce "http:///", which is
	// not a URL this function would accept back — a canonical form must be
	// canonicalizable.
	if host == "" {
		return fmt.Errorf("host %q normalizes to nothing", u.Host)
	}

	if port == "" || port == defaultPorts[u.Scheme] {
		u.Host = host
		return nil
	}
	u.Host = host + ":" + port
	return nil
}

// canonicalizePath normalizes the trailing slash and nothing else.
//
// The path is never lowercased. Most origin servers treat paths as
// case-sensitive, so /Article and /article can be different documents, and
// folding them is the aggressive kind of error that loses content.
func canonicalizePath(u *url.URL) {
	if u.Path == "" {
		// The root of a site is "/" rather than "", so that
		// https://example.com and https://example.com/ agree.
		u.Path, u.RawPath = "/", ""
		return
	}

	// A trailing slash on a non-root path is dropped, so that /article/ and
	// /article agree. Servers overwhelmingly serve both, and feeds are
	// inconsistent about which they carry — often within a single feed.
	//
	// The trim is applied to the *escaped* path and the result re-parsed,
	// rather than editing Path and discarding RawPath. Path is the decoded
	// form, in which an encoded %2F is indistinguishable from a real
	// separator; rewriting from it would silently turn /a%2Fb into /a/b and
	// address a different document.
	// TrimRight, not TrimSuffix: "/a///" has to reach its fixed point in one
	// pass. Removing a single slash per call would make Canonicalize
	// non-idempotent, and an article re-seen later would then produce a second
	// row with a second key.
	escaped := u.EscapedPath()
	trimmed := strings.TrimRight(escaped, "/")
	if trimmed == "" {
		trimmed = "/"
	}
	if trimmed == escaped {
		return
	}

	// Assign Path and RawPath the way net/url does internally. The obvious
	// alternative — url.Parse(trimmed) — is wrong here: a path beginning with
	// "//" parses as a protocol-relative URL, so "//a" would silently become
	// the host rather than the path.
	decoded, err := url.PathUnescape(trimmed)
	if err != nil {
		// Unreachable for a path that came from a parsed URL; leaving it
		// untouched is the safe response if it ever happens.
		return
	}
	u.Path, u.RawPath = decoded, ""
	if u.EscapedPath() != trimmed {
		// The escaping is not the canonical one net/url would produce, so the
		// original spelling has to be preserved explicitly.
		u.RawPath = trimmed
	}
}

// canonicalizeQuery removes tracking parameters and sorts what remains.
//
// Sorting matters: parameter order is not semantically significant, but an
// unsorted query means ?a=1&b=2 and ?b=2&a=1 produce different keys and
// therefore duplicate articles.
func canonicalizeQuery(u *url.URL) {
	if u.RawQuery == "" {
		return
	}

	values := u.Query()
	for key := range values {
		lower := strings.ToLower(key)
		if trackingParams[lower] || strings.HasPrefix(lower, utmPrefix) {
			values.Del(key)
		}
	}

	// url.Values.Encode sorts by key, which is the normalization we want.
	u.RawQuery = values.Encode()
}

// canonicalizeFragment drops the fragment, except for the "hashbang" form.
//
// A fragment addresses a location *within* a document, so two URLs differing
// only by fragment are the same article — except under the legacy #! scheme,
// where the fragment selects which document a single-page application renders.
// Those are rare now and getting rarer, but the archive holds old links.
func canonicalizeFragment(u *url.URL) {
	if strings.HasPrefix(u.Fragment, "!") {
		return
	}
	u.Fragment = ""
	u.RawFragment = ""
}

// IsTrackingParam reports whether a query parameter is stripped by
// Canonicalize. Exported for the diagnostics view that explains why two URLs
// collapsed to one article.
func IsTrackingParam(name string) bool {
	lower := strings.ToLower(name)
	return trackingParams[lower] || strings.HasPrefix(lower, utmPrefix)
}
