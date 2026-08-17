package feed_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/feed"
)

func TestParseOPML(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opml", "freshrss.opml"))
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	subs, err := feed.ParseOPML(f)
	if err != nil {
		t.Fatalf("ParseOPML() = %v", err)
	}

	want := []feed.Subscription{
		{
			FeedURL:  "https://engineering.example.com/feed.xml",
			SiteURL:  "https://engineering.example.com/",
			Title:    "Example Engineering",
			Category: "Technology",
		},
		{
			FeedURL:  "https://dev.example.org/rss",
			SiteURL:  "https://dev.example.org/",
			Title:    "Another Dev Blog",
			Category: "Technology",
		},
		{
			// Nested folders are joined so no level is discarded.
			FeedURL:  "https://news.example.net/city/feed",
			SiteURL:  "https://news.example.net/city",
			Title:    "City Desk",
			Category: "News/Local",
		},
		{
			FeedURL:  "https://news.example.net/world/feed",
			Title:    "World Desk",
			Category: "News",
		},
		{
			FeedURL:  "https://blog.example.com/atom.xml",
			SiteURL:  "https://blog.example.com/",
			Title:    "Uncategorized Blog",
			Category: "",
		},
		{
			FeedURL: "https://title-only.example.com/feed",
			Title:   "Title Only",
		},
		{
			// No name anywhere, so the URL stands in: a feed is never stored
			// with an empty title.
			FeedURL: "https://nameless.example.com/feed",
			Title:   "https://nameless.example.com/feed",
		},
	}

	if len(subs) != len(want) {
		t.Fatalf("parsed %d subscriptions, want %d:\n%+v", len(subs), len(want), subs)
	}
	for i, w := range want {
		if subs[i] != w {
			t.Errorf("subscription %d:\n got: %+v\nwant: %+v", i, subs[i], w)
		}
	}
}

// The same feed listed in two folders is one subscription. Readers produce
// this whenever a feed is filed in more than one place.
func TestParseOPMLDeduplicatesByFeedURL(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opml", "freshrss.opml"))
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	subs, err := feed.ParseOPML(f)
	if err != nil {
		t.Fatalf("ParseOPML() = %v", err)
	}

	seen := make(map[string]int)
	for _, s := range subs {
		seen[s.FeedURL]++
	}
	for url, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times, want 1", url, n)
		}
	}
}

// Bookmarks, empty folders, and URLs this service cannot poll are not
// subscriptions.
func TestParseOPMLSkipsNonFeeds(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opml", "freshrss.opml"))
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	subs, err := feed.ParseOPML(f)
	if err != nil {
		t.Fatalf("ParseOPML() = %v", err)
	}

	for _, s := range subs {
		if !strings.HasPrefix(s.FeedURL, "http://") && !strings.HasPrefix(s.FeedURL, "https://") {
			t.Errorf("subscription %q is not fetchable over HTTP", s.FeedURL)
		}
		if s.Title == "" {
			t.Errorf("subscription %q has an empty title", s.FeedURL)
		}
	}
}

func TestParseOPMLVariants(t *testing.T) {
	tests := []struct {
		name  string
		opml  string
		want  []feed.Subscription
		isErr bool
	}{
		{
			name: "flat list with no folders",
			opml: `<opml version="1.0"><body>
				<outline type="rss" text="A" xmlUrl="https://a.example.com/feed"/>
				<outline type="rss" text="B" xmlUrl="https://b.example.com/feed"/>
			</body></opml>`,
			want: []feed.Subscription{
				{FeedURL: "https://a.example.com/feed", Title: "A"},
				{FeedURL: "https://b.example.com/feed", Title: "B"},
			},
		},
		{
			name: "whitespace is trimmed",
			opml: `<opml><body>
				<outline type="rss" text="  Spaced  " xmlUrl="  https://a.example.com/feed  "/>
			</body></opml>`,
			want: []feed.Subscription{
				{FeedURL: "https://a.example.com/feed", Title: "Spaced"},
			},
		},
		{
			name: "feed URL query parameters are preserved",
			// Not canonicalized: on a feed endpoint these select which feed is
			// served, unlike the tracking parameters stripped from articles.
			opml: `<opml><body>
				<outline type="rss" text="Filtered" xmlUrl="https://example.com/feed?format=rss&amp;ref=main&amp;utm_source=x"/>
			</body></opml>`,
			want: []feed.Subscription{
				{FeedURL: "https://example.com/feed?format=rss&ref=main&utm_source=x", Title: "Filtered"},
			},
		},
		{
			name: "empty body",
			opml: `<opml version="2.0"><head><title>Empty</title></head><body></body></opml>`,
			want: nil,
		},
		{
			name:  "not XML at all",
			opml:  `this is not OPML`,
			isErr: true,
		},
		{
			name:  "truncated document",
			opml:  `<opml><body><outline type="rss" xmlUrl="https://a.example.com/feed"`,
			isErr: true,
		},
		{
			name:  "unsupported encoding is reported rather than mangled",
			opml:  `<?xml version="1.0" encoding="ISO-8859-1"?><opml><body></body></opml>`,
			isErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := feed.ParseOPML(strings.NewReader(tt.opml))
			if tt.isErr {
				if err == nil {
					t.Fatalf("ParseOPML() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOPML() = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parsed %d subscriptions, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("subscription %d:\n got: %+v\nwant: %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
