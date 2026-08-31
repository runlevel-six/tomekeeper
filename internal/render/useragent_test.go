package render

import "testing"

// The browser-backed test for this lives in render_integration_test.go and needs a
// real Chrome. The lookup it depends on does not, and it is the part a domain rule
// changes.
func TestUserAgentForHost(t *testing.T) {
	r := &Renderer{userAgent: "tomekeeper/test"}

	const disguised = "Mozilla/5.0 (compatible; tomekeeper/test; +https://example.com)"
	r.SetHostUserAgent("picky.example.com", disguised)

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"the overridden host", "https://picky.example.com/a", disguised},
		{"a port is part of the host key", "https://picky.example.com:8443/a", "tomekeeper/test"},
		{"any other host", "https://plain.example.com/a", "tomekeeper/test"},
		{"a URL that will not parse", "://nonsense", "tomekeeper/test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.userAgentFor(tc.url); got != tc.want {
				t.Errorf("userAgentFor(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}

	// Clearing it restores the default, so emptying the field in a rule works.
	r.SetHostUserAgent("picky.example.com", "")
	if got, want := r.userAgentFor("https://picky.example.com/a"), "tomekeeper/test"; got != want {
		t.Errorf("userAgentFor after reset = %q, want %q", got, want)
	}

	// A nil Renderer is the ordinary deployment, and startup calls this on it.
	var absent *Renderer
	absent.SetHostUserAgent("picky.example.com", disguised)
}
