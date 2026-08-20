package asseturl

import (
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSecret = "a secret that is only for this test"

const testPath = "/assets/sha256/a1/b2/a1b2c3d4e5f6.avif"

// signer builds a signer, failing the test rather than returning an error, because
// every case here needs one and none of them is about construction.
func signer(t *testing.T, ttl time.Duration) *Signer {
	t.Helper()

	s, err := NewSigner([]byte(testSecret), ttl)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}
	return s
}

// parse splits a signed URL back into the two things Verify is given, which is
// what the handler does with an incoming request.
func parse(t *testing.T, signed string) (path, sig string) {
	t.Helper()

	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parsing the signed URL %q = %v", signed, err)
	}
	return u.Path, u.Query().Get(SignatureParam)
}

// expiryOf pulls the expiry half out of a signature value.
func expiryOf(t *testing.T, sig string) int64 {
	t.Helper()

	exp, _, found := strings.Cut(sig, signatureSeparator)
	if !found {
		t.Fatalf("the signature %q carries no expiry", sig)
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		t.Fatalf("the expiry in %q is not a number: %v", sig, err)
	}
	return n
}

// resign rebuilds a signature value with a different expiry, keeping the MAC.
func resign(t *testing.T, sig string, expiry int64) string {
	t.Helper()

	_, mac, found := strings.Cut(sig, signatureSeparator)
	if !found {
		t.Fatalf("the signature %q carries no MAC", sig)
	}
	return strconv.FormatInt(expiry, 10) + signatureSeparator + mac
}

func TestASignedURLVerifies(t *testing.T) {
	s := signer(t, DefaultTTL)

	signed := s.Sign(testPath)
	if !strings.HasPrefix(signed, testPath+"?") {
		t.Fatalf("Sign() = %q, want the path with a query appended", signed)
	}

	if !s.Verify(parse(t, signed)) {
		t.Errorf("a freshly signed URL does not verify: %q", signed)
	}
}

// The signature covers the path, so one cannot be moved to another image.
//
// This is the guard that makes a signed URL a capability for one asset rather than
// for the whole archive. It fails if the MAC stops covering the path.
func TestASignatureCannotBeMovedToAnotherAsset(t *testing.T) {
	s := signer(t, DefaultTTL)

	_, sig := parse(t, s.Sign(testPath))

	const other = "/assets/sha256/ff/ee/ffeeddccbbaa.avif"
	if s.Verify(other, sig) {
		t.Error("a signature issued for one asset verifies against another")
	}
}

// The signature covers the expiry, so a stale URL cannot be extended.
//
// It fails if the MAC stops covering the expiry — at which point any signed URL
// would be valid forever, which is the whole reason there is an expiry.
func TestAnExpiryCannotBeExtended(t *testing.T) {
	s := signer(t, time.Minute)

	_, sig := parse(t, s.Sign(testPath))

	extended := resign(t, sig, expiryOf(t, sig)+86400)

	if s.Verify(testPath, extended) {
		t.Error("an expiry moved a day into the future still verifies")
	}
}

// The delimiter between the path and the expiry has to make the pair unambiguous.
//
// Without one, "/a/b" expiring at 12 and "/a/b1" expiring at 2 sign the same bytes,
// and either signature works for both. Contrived paths, but the property is the
// reason the newline is there and nothing else would catch its removal.
func TestThePathAndExpiryCannotRunTogether(t *testing.T) {
	s := signer(t, DefaultTTL)

	first := s.signature("/a/b", 12)
	second := s.signature("/a/b1", 2)

	if first == second {
		t.Error("two different path-and-expiry pairs produce the same signature")
	}
}

func TestAnExpiredURLIsRefused(t *testing.T) {
	// A ttl this short is already spent by the time Verify runs, which is the point:
	// nothing here sleeps, and nothing depends on how fast the test host is.
	s := signer(t, time.Nanosecond)

	signed := s.Sign(testPath)
	if s.Verify(parse(t, signed)) {
		t.Errorf("an expired URL verifies: %q", signed)
	}
}

func TestAMissingOrJunkSignatureIsRefused(t *testing.T) {
	s := signer(t, DefaultTTL)
	_, sig := parse(t, s.Sign(testPath))
	_, mac, _ := strings.Cut(sig, signatureSeparator)

	cases := map[string]struct{ path, sig string }{
		"no signature at all": {testPath, ""},
		"no path":             {"", sig},
		"expiry only":         {testPath, strconv.FormatInt(expiryOf(t, sig), 10)},
		"mac with no expiry":  {testPath, mac},
		"junk expiry":         {testPath, "tomorrow" + signatureSeparator + mac},
		"junk mac":            {testPath, resign(t, "0"+signatureSeparator+"not-base64-at-all", expiryOf(t, sig))},
		"no separator":        {testPath, strings.Replace(sig, signatureSeparator, "", 1)},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if s.Verify(c.path, c.sig) {
				t.Error("verified a request that carried no usable signature")
			}
		})
	}
}

// A signature from one secret must not verify under another.
//
// This is what makes rotating TOME_SESSION_KEY invalidate outstanding image URLs,
// which the package comment promises.
func TestASignatureIsBoundToItsSecret(t *testing.T) {
	mine := signer(t, DefaultTTL)

	theirs, err := NewSigner([]byte("a different secret entirely"), DefaultTTL)
	if err != nil {
		t.Fatalf("NewSigner() = %v", err)
	}

	_, sig := parse(t, mine.Sign(testPath))
	if theirs.Verify(testPath, sig) {
		t.Error("a signature verifies under a secret that did not produce it")
	}
}

// Signing something that already carries a query is refused rather than mangled: it
// would mean this is being used for something it was not written for, and appending
// a second "?" would produce a URL that fetches nothing.
func TestSigningLeavesAnUnexpectedURLAlone(t *testing.T) {
	s := signer(t, DefaultTTL)

	for _, in := range []string{"", "/assets/x.avif?already=here", "/assets/x.avif#frag"} {
		if out := s.Sign(in); out != in {
			t.Errorf("Sign(%q) = %q, want it returned unchanged", in, out)
		}
	}
}

// A signed URL has to survive being written into an HTML attribute byte for byte.
//
// This is the property the single-parameter form exists for, and it is not
// hypothetical: with a separate expiry and signature the ampersand between them
// serializes as `&amp;`, and whether the image loads then depends on the client's
// HTML parser rather than on this code. It was found by a test fetching the URL out
// of a rendered body and getting a 303.
func TestASignedURLSurvivesAnHTMLAttribute(t *testing.T) {
	s := signer(t, DefaultTTL)

	signed := s.Sign(testPath)
	escaped := template.HTMLEscapeString(signed)

	if escaped != signed {
		t.Errorf("HTML escaping rewrites the signed URL:\n  signed  %s\n  escaped %s", signed, escaped)
	}
	if !s.Verify(parse(t, escaped)) {
		t.Errorf("the signed URL does not verify after a round trip through an attribute: %s", escaped)
	}
}

func TestConstructionRefusesUnusableInput(t *testing.T) {
	if _, err := NewSigner(nil, DefaultTTL); err == nil {
		t.Error("NewSigner accepted an empty secret")
	}
	if _, err := NewSigner([]byte(testSecret), 0); err == nil {
		t.Error("NewSigner accepted a zero ttl")
	}
	if _, err := NewSigner([]byte(testSecret), -time.Hour); err == nil {
		t.Error("NewSigner accepted a negative ttl")
	}
}
