package store_test

import (
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/dbtest"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Each audit lens finds the thing it is for, and leaves alone the things it is not.
//
// The negative halves carry the weight. Every one of them is a real article from a real
// archive that an earlier version of these queries flagged, and the reason none of the
// lenses became a rejection rung: a comic has no prose, a hand-written selector cannot
// wander, and an imported body may be the only surviving copy of a page that is gone.
func TestSuspectBodiesFindsABodyThatIgnoresItsTitle(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// The case that prompted the whole thing: a consent dialog stored as an article.
	// Nothing in it mentions infrastructure, code, or moving.
	consent := newArticleWithBody(t, s, body{
		url:   "https://example.com/whats-left-for-infrastructure-as-code",
		title: "What's left for infrastructure as code after AI moves in",
		rung:  "readability",
		text: "Cookie consent preference center. When you visit any of our websites it may " +
			"store or retrieve information on your browser, mostly in the form of cookies. " +
			"This information might be about you, your preferences or your device.",
	})

	// An ordinary article, whose body talks about what its title says.
	ordinary := newArticleWithBody(t, s, body{
		url:   "https://example.com/engineering-leadership",
		title: "Engineering leadership when the cost of code approaches zero",
		rung:  "trafilatura",
		text: "Engineering leadership has always been about judgement rather than typing, " +
			"and when the cost of code approaches zero the judgement is all that is left.",
	})

	// A comic. Its body is a picture, so it has no prose to match its title — and
	// flagging it would bury the list in exactly the articles this archive is most
	// careful about. Excluded by rung, not by luck.
	comic := newArticleWithBody(t, s, body{
		url:   "https://example.com/comic/breaking-up-p13",
		title: "Breaking up p13",
		rung:  "page_images",
		text:  "",
	})

	// A hand-written selector cannot wander onto the wrong block, so this rung is not
	// worth looking at — 217 for 217 on a real archive.
	ruled := newArticleWithBody(t, s, body{
		url:   "https://example.com/ruled",
		title: "Something else entirely here",
		rung:  "domain_rule",
		text:  "A body with no overlap at all, reached by a selector somebody wrote.",
	})

	// An import, which no re-extraction can touch.
	imported := newArticleWithBody(t, s, body{
		url:       "https://example.com/imported",
		title:     "Another unrelated title string",
		rung:      "imported",
		immutable: true,
		text:      "A body with nothing in common with its title, from a source now gone.",
	})

	found, err := s.SuspectBodies(ctx, 50)
	if err != nil {
		t.Fatalf("SuspectBodies() = %v", err)
	}

	got := map[store.ArticleID]store.SuspectBody{}
	for _, b := range found {
		got[b.ArticleID] = b
	}

	if _, ok := got[consent]; !ok {
		t.Errorf("the consent dialog was not flagged; got %d finding(s): %v", len(found), ids(found))
	}
	for name, id := range map[string]store.ArticleID{
		"an ordinary article":     ordinary,
		"a comic (page_images)":   comic,
		"a domain_rule body":      ruled,
		"an imported (immutable)": imported,
	} {
		if _, ok := got[id]; ok {
			t.Errorf("%s was flagged, and must not be", name)
		}
	}

	// The report has to say enough to act on without a second query.
	if b := got[consent]; b.Extractor != "readability" || b.TitleWords < 3 || b.URL == "" {
		t.Errorf("the finding is missing detail: %+v", b)
	}
}

// A title with too little in it to measure is not evidence of anything.
func TestSuspectBodiesIgnoresATitleWithNothingDistinctiveInIt(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	// Two distinctive words, one short one. Under the floor, so no finding — a
	// two-word title that happens to miss says nothing about the body.
	newArticleWithBody(t, s, body{
		url:   "https://example.com/short-title",
		title: "The Heap and it",
		rung:  "readability",
		text:  "A body about something completely different, sharing no word with the title.",
	})

	found, err := s.SuspectBodies(t.Context(), 50)
	if err != nil {
		t.Fatalf("SuspectBodies() = %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a title with fewer than three distinctive words was judged anyway: %v", ids(found))
	}
}

func TestSharedBodiesFindsAWallServedAsTwoArticles(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	// The same sign-in page stored as the body of two different articles. Imported,
	// deliberately: excluding immutable bodies here would lose the only real finding
	// this query has made on a live archive.
	const wall = "Midway Authentication Portal. Sign in. Sign in with Midway. " +
		"Why am I seeing this page? Your session may have expired or you may not have " +
		"permission to view the resource you requested from this network location."
	first := newArticleWithBody(t, s, body{
		url: "https://example.com/one", title: "Internal runbook one",
		rung: "imported", immutable: true, text: wall,
	})
	second := newArticleWithBody(t, s, body{
		url: "https://example.com/two", title: "Internal runbook two",
		rung: "imported", immutable: true, text: wall,
	})

	// Two articles whose bodies merely resemble each other are not a finding.
	newArticleWithBody(t, s, body{
		url: "https://example.com/three", title: "A real article",
		rung: "trafilatura", text: strings.Repeat("Genuine prose about a real subject. ", 4),
	})
	newArticleWithBody(t, s, body{
		url: "https://example.com/four", title: "Another real article",
		rung: "trafilatura", text: strings.Repeat("Genuine prose about a real subject! ", 4),
	})

	found, err := s.SharedBodies(ctx, 50)
	if err != nil {
		t.Fatalf("SharedBodies() = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("want exactly the wall, got %d group(s): %+v", len(found), found)
	}
	g := found[0]
	if len(g.ArticleIDs) != 2 || g.ArticleIDs[0] != first || g.ArticleIDs[1] != second {
		t.Errorf("the group names %v, want [%d %d]", g.ArticleIDs, first, second)
	}
	if !g.Immutable {
		t.Error("the group is not marked immutable, so the report would suggest a remedy that cannot work")
	}
	if !strings.Contains(g.Opening, "Midway") {
		t.Errorf("the opening does not show what the shared body is: %q", g.Opening)
	}
}

// Short bodies collide for dull reasons, and a report full of them is a report nobody
// reads.
func TestSharedBodiesIgnoresShortCollisions(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)

	const stub = "Read more at the original site."
	newArticleWithBody(t, s, body{url: "https://example.com/a", title: "A", rung: "feed_body", text: stub})
	newArticleWithBody(t, s, body{url: "https://example.com/b", title: "B", rung: "feed_body", text: stub})

	found, err := s.SharedBodies(t.Context(), 50)
	if err != nil {
		t.Fatalf("SharedBodies() = %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a twenty-word stub was reported as a shared body: %+v", found)
	}
}

func TestPlaceholderTitlesFindsURLsAndEncodedFilenames(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	withBody := newArticleWithBody(t, s, body{
		url:   "https://example.com/datapath.pdf",
		title: "eBPF%20and%20the%20Cilium%20Datapath.pdf",
		rung:  "trafilatura",
		text:  "A long and genuine body, sixteen thousand words of it in the real case.",
	})
	bodyless := newTitled(t, s, "https://console.example.com/ecs/home?region=us-east-1",
		"https://console.example.com/ecs/home?region=us-east-1")
	newTitled(t, s, "https://example.com/fine", "An ordinary title")
	// A percent sign that is not an escape. Leaving it alone is the point: it may be a
	// title that genuinely contains one.
	newTitled(t, s, "https://example.com/percent", "100% legitimate as a title")

	found, err := s.PlaceholderTitles(ctx, 50)
	if err != nil {
		t.Fatalf("PlaceholderTitles() = %v", err)
	}

	got := map[store.ArticleID]store.PlaceholderTitle{}
	for _, t := range found {
		got[t.ArticleID] = t
	}
	if len(found) != 2 {
		t.Fatalf("want exactly the two placeholders, got %d: %+v", len(found), found)
	}
	if !got[withBody].HasBody {
		t.Error("the one with a body is not marked as such, so the report cannot say reextract fixes it")
	}
	if got[bodyless].HasBody {
		t.Error("the bodyless one is marked as having a body, so the report would promise a fix that cannot happen")
	}
}

func ids(bs []store.SuspectBody) []store.ArticleID {
	out := make([]store.ArticleID, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ArticleID)
	}
	return out
}

// body is one article and its stored body, which every lens here needs together.
type body struct {
	url, title, rung, text string
	immutable              bool
}

func newArticleWithBody(t *testing.T, s *store.Store, b body) store.ArticleID {
	t.Helper()

	id := newTitled(t, s, b.url, b.title)
	// An imported body records where it came from, and is the only kind that is
	// immutable — it may be the only surviving copy of a page that is gone.
	origin := store.OriginFetched
	if b.immutable {
		origin = "import:wallabag"
	}
	insertBody(t, s, id, store.ContentParams{
		ExtractorName: b.rung,
		ContentOrigin: origin,
		Immutable:     b.immutable,
		HTML:          "<p>" + b.text + "</p>",
		Text:          b.text,
		WordCount:     len(strings.Fields(b.text)),
	})
	return id
}

func newTitled(t *testing.T, s *store.Store, url, title string) store.ArticleID {
	t.Helper()

	id, _, err := s.UpsertArticle(t.Context(), store.ArticleParams{URLCanonical: url, Title: title})
	if err != nil {
		t.Fatalf("UpsertArticle() = %v", err)
	}
	return id
}

// A title that is a URL counts as a gap, so the page gets to replace it.
//
// UpdateArticleMetadata fills gaps only, which is right — a feed's title is a choice
// somebody made, and the page's is not automatically better. But a URL is not a choice
// anybody made, and treating it as one left twelve articles here titled with their own
// address, one of them 16,249 words under `eBPF%20and%20the%20Cilium%20Datapath.pdf`.
func TestExtractionReplacesATitleThatIsNotATitle(t *testing.T) {
	_, s, _ := dbtest.SetupWithUser(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		start string
		page  string
		want  string
	}{
		{
			name:  "a URL is replaced by what the page calls itself",
			start: "https://docs.aws.amazon.com/cdk/api/latest/docs/aws-eks-readme.html",
			page:  "Amazon EKS Construct Library",
			want:  "Amazon EKS Construct Library",
		},
		{
			name:  "an encoded filename is replaced too",
			start: "eBPF%20and%20the%20Cilium%20Datapath.pdf",
			page:  "eBPF and the Cilium Datapath",
			want:  "eBPF and the Cilium Datapath",
		},
		{
			name:  "a real title is kept, however plain",
			start: "Notes",
			page:  "Notes — A Much Longer And Fancier Title From The Page",
			want:  "Notes",
		},
		{
			name:  "a placeholder is kept when the page offers nothing better",
			start: "https://example.com/no-title-anywhere",
			page:  "",
			want:  "https://example.com/no-title-anywhere",
		},
		{
			name:  "an empty title is filled, as it always was",
			start: "",
			page:  "What the page calls itself",
			want:  "What the page calls itself",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := newTitled(t, s, "https://example.com/"+strings.ReplaceAll(tc.name, " ", "-"), tc.start)
			if err := s.UpdateArticleMetadata(ctx, id, store.ArticleParams{Title: tc.page}); err != nil {
				t.Fatalf("UpdateArticleMetadata() = %v", err)
			}
			a, err := s.GetArticle(ctx, id)
			if err != nil {
				t.Fatalf("GetArticle() = %v", err)
			}
			if a.Title != tc.want {
				t.Errorf("title is %q, want %q", a.Title, tc.want)
			}
		})
	}
}

// The Go predicate and the SQL one have to agree, because the importer uses one and
// the report uses the other.
func TestTitleIsPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  bool
	}{
		{"https://example.com/thing", true},
		{"http://en.clouddesignpattern.org/index.php/Main_Page", true},
		{"ftp://files.example.com/x", true},
		{"eBPF%20and%20the%20Cilium%20Datapath.pdf", true},
		{"  https://example.com/leading-space  ", true},
		{"An ordinary title", false},
		{"", false},
		{"100% legitimate as a title", false},
		{"Cost: 50%–60% of budget", false},
		// A URL inside a title is not a title that *is* a URL. Anchoring matters:
		// without it, "Reviewing https://example.com" would be discarded.
		{"Reviewing https://example.com and what it means", false},
	} {
		if got := store.TitleIsPlaceholder(tc.title); got != tc.want {
			t.Errorf("TitleIsPlaceholder(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}
