package server_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/store"
)

// categoryID reads a category's id out of the index page, so these tests navigate the
// way a reader does rather than reaching into the database for a number.
func categoryID(t *testing.T, rd *reader, name string) string {
	t.Helper()

	body := rd.body("/categories")
	// The row's own management link is the only place the id appears.
	i := strings.Index(body, ">"+name+"<")
	if i < 0 {
		t.Fatalf("no category named %q on the index:\n%s", name, body)
	}
	rest := body[i:]
	const marker = `/categories?delete=`
	j := strings.Index(rest, marker)
	if j < 0 {
		t.Fatalf("category %q has no delete link, so it is not managed:\n%s", name, rest[:min(len(rest), 400)])
	}
	rest = rest[j+len(marker):]
	k := strings.IndexAny(rest, `"&`)
	if k < 0 {
		t.Fatalf("malformed delete link")
	}
	return rest[:k]
}

func createCategory(t *testing.T, rd *reader, name string) {
	t.Helper()
	if rec := rd.do(http.MethodPost, "/categories/new", url.Values{"name": {name}}); rec.Code != http.StatusOK {
		t.Fatalf("POST /categories/new (%q) = %d, want 200\n%s", name, rec.Code, rec.Body.String())
	}
}

// An empty category has to be creatable and visible, which is the whole thing free
// text could not do: there was no object to make, so "add a folder then fill it" was
// a sequence a reader could not perform.
func TestAnEmptyCategoryCanBeCreatedAndSeen(t *testing.T) {
	rd, _ := readingFixture(t)

	createCategory(t, rd, "Reading later")

	body := rd.body("/categories")
	if !strings.Contains(body, "Reading later") {
		t.Errorf("the new category is not on the index:\n%s", body)
	}
	if !strings.Contains(body, "0 feeds") {
		t.Errorf("the new category does not report being empty:\n%s", body)
	}
}

// A name already taken is refused by name, because the useful thing to say is which
// one — and it is a name they can see in the list below the message.
func TestADuplicateCategoryNameIsRefusedByName(t *testing.T) {
	rd, _ := readingFixture(t)

	createCategory(t, rd, "Tech")

	rec := rd.do(http.MethodPost, "/categories/new", url.Values{"name": {"Tech"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("POST a duplicate name = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "called Tech") {
		t.Errorf("the refusal does not name the category:\n%s", rec.Body.String())
	}
	// And a blank one says what is missing rather than failing silently.
	if rec := rd.do(http.MethodPost, "/categories/new", url.Values{"name": {"   "}}); rec.Code != http.StatusBadRequest {
		t.Errorf("POST a blank name = %d, want 400", rec.Code)
	}
}

// The nameless bucket must not be manageable: it is the absence of a category, not
// one named for absence, so there is nothing to rename and nothing to delete.
func TestTheNamelessBucketOffersNoManagementControls(t *testing.T) {
	rd, tr := readingFixture(t)

	// The fixture's own feed carries no category, so the bucket exists.
	if err := tr.store.SetFeedCategory(t.Context(), tr.alice, tr.aliceFeed, nil); err != nil {
		t.Fatalf("SetFeedCategory() = %v", err)
	}

	body := rd.body("/categories")
	heading := categoryHeadingFor(t, body)
	if heading == "" {
		t.Fatalf("the index has no nameless bucket:\n%s", body)
	}
	// Its row must carry no rename or delete link. Checked on the row rather than the
	// page, because a managed category elsewhere legitimately has both.
	if strings.Contains(heading, "/categories?edit=") || strings.Contains(heading, "/categories?delete=") {
		t.Errorf("the nameless bucket offers management controls:\n%s", heading)
	}
}

// categoryHeadingFor returns the markup of the row for the category with no name.
func categoryHeadingFor(t *testing.T, body string) string {
	t.Helper()

	// The heading the server renders for a nameless category; taken from the page so
	// this test does not restate it.
	for _, label := range []string{"Uncategorized", "No category", "Unfiled"} {
		if i := strings.Index(body, label); i >= 0 {
			rest := body[i:]
			if j := strings.Index(rest, "</li>"); j >= 0 {
				return rest[:j]
			}
			return rest
		}
	}
	return ""
}

// **The assertion the delete design rests on, from the reader's side: no answer to
// the prompt deletes an article.** Every disposition, in one test, so a fourth cannot
// be added without noticing the property.
func TestNoAnswerToTheDeletePromptLosesAnArticle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition string
		wantFeeds   int
	}{
		{"leave uncategorized", "uncategorized", 1},
		{"move elsewhere", "move", 1},
		{"unsubscribe", "unsubscribe", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd, tr := readingFixture(t)
			ctx := t.Context()

			createCategory(t, rd, "Doomed")
			createCategory(t, rd, "Elsewhere")

			doomedID := categoryID(t, rd, "Doomed")
			elsewhereID := categoryID(t, rd, "Elsewhere")

			// The fixture's feed and its article go into the doomed category.
			cid, err := tr.store.CategoryByName(ctx, tr.alice, "Doomed")
			if err != nil {
				t.Fatalf("CategoryByName() = %v", err)
			}
			if err := tr.store.SetFeedCategory(ctx, tr.alice, tr.aliceFeed, &cid); err != nil {
				t.Fatalf("SetFeedCategory() = %v", err)
			}

			form := url.Values{"id": {doomedID}, "disposition": {tc.disposition}}
			if tc.disposition == "move" {
				form.Set("into", elsewhereID)
			}
			rec := rd.do(http.MethodPost, "/categories/delete", form)
			if rec.Code != http.StatusOK {
				t.Fatalf("POST /categories/delete = %d, want 200\n%s", rec.Code, rec.Body.String())
			}

			// The article and its body survive every disposition. This is the
			// promise; the assertions below it are about what a reader then sees.
			if _, err := tr.store.GetArticle(ctx, tr.aliceOnly); err != nil {
				t.Errorf("the article is gone after %q: %v", tc.disposition, err)
			}
			if _, err := tr.store.CurrentContent(ctx, tr.aliceOnly); err != nil {
				t.Errorf("the article's body is gone after %q: %v", tc.disposition, err)
			}

			// Whether it is still *listed* is a different question, and unsubscribing
			// has always answered it differently: an article is visible when a feed
			// points at it or the reader has acted on it, so dropping the
			// subscription hides anything never opened. Asserted rather than glossed
			// over — it is the residue `tome prune` exists to answer, and a test that
			// expected the article to stay listed would have been asserting a bug.
			listed := strings.Contains(rd.body("/all"), "Alice")
			wantListed := tc.disposition != "unsubscribe"
			if listed != wantListed {
				t.Errorf("article listed = %v after %q, want %v", listed, tc.disposition, wantListed)
			}

			feeds, err := tr.store.ListFeeds(ctx, tr.alice)
			if err != nil {
				t.Fatalf("ListFeeds() = %v", err)
			}
			if len(feeds) != tc.wantFeeds {
				t.Errorf("%d feeds remain after %q, want %d", len(feeds), tc.disposition, tc.wantFeeds)
			}

			// And the message tells the whole truth. "Nothing was deleted" alone is
			// accurate and misleading: the articles are on disk and anything never
			// opened has stopped being listed. A reader told only the reassuring half
			// concludes the interface lost their archive.
			if tc.disposition == "unsubscribe" {
				out := rec.Body.String()
				if !strings.Contains(out, "Nothing they archived was deleted") {
					t.Errorf("the unsubscribe outcome does not say the archive was kept:\n%s", out)
				}
				if !strings.Contains(out, "no longer listed") {
					t.Errorf("the unsubscribe outcome does not admit the articles stop being listed:\n%s", out)
				}
			}
		})
	}
}

// A category belonging to somebody else is not found, never forbidden: a distinct
// refusal would confirm it exists.
func TestManagingAnotherReadersCategoryIsNotFound(t *testing.T) {
	rd, tr := readingFixture(t)

	id, err := tr.store.CreateCategory(t.Context(), tr.bob, "Bob's folder")
	if err != nil {
		t.Fatalf("CreateCategory() = %v", err)
	}
	raw := strconv.FormatInt(int64(id), 10)

	for _, path := range []string{"/categories/rename", "/categories/delete"} {
		form := url.Values{"id": {raw}, "name": {"Mine"}, "disposition": {"uncategorized"}}
		if rec := rd.do(http.MethodPost, path, form); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s against another reader's category = %d, want 404", path, rec.Code)
		}
	}
	// Still theirs.
	if _, err := tr.store.CategoryByName(t.Context(), tr.bob, "Bob's folder"); err != nil {
		t.Errorf("the other reader's category was affected: %v", err)
	}
}

// The prompt has to say what will happen to the feeds, and has to say that the
// articles are kept — "delete" invites the opposite assumption, and being told is the
// only way a reader learns otherwise.
func TestTheDeletePromptSaysWhatItWillAndWillNotDo(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	createCategory(t, rd, "Doomed")
	cid, err := tr.store.CategoryByName(ctx, tr.alice, "Doomed")
	if err != nil {
		t.Fatalf("CategoryByName() = %v", err)
	}
	if err := tr.store.SetFeedCategory(ctx, tr.alice, tr.aliceFeed, &cid); err != nil {
		t.Fatalf("SetFeedCategory() = %v", err)
	}

	body := rd.body("/categories?delete=" + strconv.FormatInt(int64(cid), 10))

	for _, want := range []string{
		"Delete", "1 feed",
		"every article they have archived is kept",
		`value="uncategorized"`,
		`value="unsubscribe"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the delete prompt does not contain %q:\n%s", want, body)
		}
	}

	// With nothing else to move them to, the move option is not offered: an option
	// leading to an empty picker is worse than no option.
	if strings.Contains(body, `value="move"`) {
		t.Errorf("the prompt offered a move with nowhere to move to:\n%s", body)
	}

	createCategory(t, rd, "Elsewhere")
	body = rd.body("/categories?delete=" + strconv.FormatInt(int64(cid), 10))
	if !strings.Contains(body, `value="move"`) {
		t.Errorf("the prompt does not offer a move now that there is somewhere to go:\n%s", body)
	}
}

// A rename must not disturb the feeds, and the page must say so — a reader has no
// other way to know that renaming a folder is not a bulk edit of its contents.
func TestRenamingFromThePageKeepsTheFeeds(t *testing.T) {
	rd, tr := readingFixture(t)
	ctx := t.Context()

	createCategory(t, rd, "Tech")
	cid, err := tr.store.CategoryByName(ctx, tr.alice, "Tech")
	if err != nil {
		t.Fatalf("CategoryByName() = %v", err)
	}
	if err := tr.store.SetFeedCategory(ctx, tr.alice, tr.aliceFeed, &cid); err != nil {
		t.Fatalf("SetFeedCategory() = %v", err)
	}

	rec := rd.do(http.MethodPost, "/categories/rename",
		url.Values{"id": {strconv.FormatInt(int64(cid), 10)}, "name": {"Technology"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /categories/rename = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	feed, err := tr.store.GetFeed(ctx, tr.alice, tr.aliceFeed)
	if err != nil {
		t.Fatalf("GetFeed() = %v", err)
	}
	if feed.Category != "Technology" {
		t.Errorf("the feed reports category %q after the rename, want %q", feed.Category, "Technology")
	}
	if feed.CategoryID != cid {
		t.Errorf("the feed moved to category %d, want %d — a rename must not refile anything",
			feed.CategoryID, cid)
	}

	// And the stream keyed by the new name finds the article.
	if body := rd.body("/categories?name=Technology"); !strings.Contains(body, "Alice") {
		t.Errorf("the renamed category's stream is empty:\n%s", body)
	}
}

var _ = store.DispositionMove
