package server_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// The lead image of an article appears beside its headline in every list.
//
// One test across the reading list and the search results rather than one each:
// they are two templates showing the same fact, and the way this breaks is that one
// of them is changed and the other is forgotten.
func TestListsShowTheLeadImage(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	const sha = "1eadaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fsPath := "assets/1e/ad/" + sha + ".avif"

	if _, err := tr.store.UpsertAsset(ctx, store.Asset{
		SHA256: sha, MediaType: "image/avif", ByteSize: 44_000,
		Width: 1200, Height: 800, FSPath: fsPath,
	}); err != nil {
		t.Fatalf("UpsertAsset() = %v", err)
	}
	if err := tr.store.LinkAsset(ctx, tr.aliceOnly, sha); err != nil {
		t.Fatalf("LinkAsset() = %v", err)
	}
	if err := tr.store.UpdateContentHTML(ctx, tr.aliceOnly, store.Household(),
		`<p>A distinctive alpaca passage that only Alice can read.</p>`+
			`<img src="/`+fsPath+`">`, sha); err != nil {
		t.Fatalf("UpdateContentHTML() = %v", err)
	}

	for _, page := range []string{"/", "/all", "/search?q=alpaca"} {
		t.Run(page, func(t *testing.T) {
			body := rd.body(page)

			if !strings.Contains(body, `src="/`+fsPath+`"`) {
				t.Errorf("%s does not show the archived picture:\n%s", page, body)
			}
			// From this archive, never from the site it came from. A list that
			// hotlinked would report each reader's position to every publisher.
			if strings.Contains(body, `<img src="http`) {
				t.Errorf("%s loads an image from somewhere other than the archive", page)
			}
			// Lazily, or a list of fifty pictures is fifty requests before the
			// reader has scrolled anywhere.
			if !strings.Contains(body, `loading="lazy"`) {
				t.Errorf("%s loads its thumbnails eagerly", page)
			}
			// The headline says what the article is; a described thumbnail beside it
			// says it twice.
			if !strings.Contains(body, `alt=""`) {
				t.Errorf("%s does not mark the thumbnail decorative", page)
			}
			// One link per article, whatever a reader is navigating with.
			if !strings.Contains(body, `aria-hidden="true"`) {
				t.Errorf("%s exposes the thumbnail as a second link", page)
			}
		})
	}

	// The link under the picture goes where the headline goes, carrying the list.
	// A thumbnail that opened a different article than the words beside it would be
	// worse than one that did nothing at all.
	href := `href="/articles/` + strconv.FormatInt(int64(tr.aliceOnly), 10) + `?from=unread"`
	if strings.Count(rd.body("/"), href) != 2 {
		t.Errorf("the picture and the headline do not both link to the article:\n%s", rd.body("/"))
	}

	// And an article that leads with nothing shows nothing: no empty frame, no
	// placeholder, no reserved column.
	if err := tr.store.UpdateContentHTML(ctx, tr.aliceOnly, store.Household(),
		"<p>A distinctive alpaca passage that only Alice can read.</p>", ""); err != nil {
		t.Fatalf("UpdateContentHTML() = %v", err)
	}
	for _, page := range []string{"/", "/search?q=alpaca"} {
		if body := rd.body(page); strings.Contains(body, `class="thumb"`) {
			t.Errorf("%s draws a thumbnail for an article that has no picture", page)
		}
	}
}
