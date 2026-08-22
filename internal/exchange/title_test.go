package exchange

import "testing"

// Wallabag keeps the URL as the title when it found none, and for a link to a document
// that URL is the filename — so the escapes arrive in the archive as the title.
func TestDecodeTitle(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"eBPF%20and%20the%20Cilium%20Datapath.pdf", "eBPF and the Cilium Datapath.pdf"},
		{"Notes%2C%20revised", "Notes, revised"},
		{"C%2B%2B%20for%20the%20impatient", "C++ for the impatient"},
		// A plus is a plus. QueryUnescape would read it as a space, which is wrong in a
		// filename — and the case needs an escape in it too, or it returns before the
		// decoder ever runs and proves nothing about which decoder was used.
		{"go1.26+notes%20revised", "go1.26+notes revised"},
		{"go1.26+notes", "go1.26+notes"},
		{"An ordinary title", "An ordinary title"},
		{"  padded  ", "padded"},
		{"", ""},
		// A stray percent is not an escape. Leaving it be is the honest answer: it may
		// be a title that genuinely contains one.
		{"100% legitimate", "100% legitimate"},
		{"50%–60% of budget", "50%–60% of budget"},
		// A whole URL is deliberately left alone. "home" is not a better title than the
		// address, and extraction replaces a URL-shaped title from the page itself.
		{"https://console.example.com/ecs/home", "https://console.example.com/ecs/home"},
	} {
		if got := decodeTitle(tc.in); got != tc.want {
			t.Errorf("decodeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
