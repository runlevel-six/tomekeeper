package extract_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/extract"
)

var srcPattern = regexp.MustCompile(`<img[^>]*src="([^"]+)"`)

func imageSources(html string) []string {
	var out []string
	for _, m := range srcPattern.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// The whole rung rests on telling a comic from the navigation around it, on a
// page where the navigation outnumbers the content twenty to one. This asserts
// which images are chosen, not merely that some were.
func TestPageImagesSelectsBySlug(t *testing.T) {
	const page = `<html><body>
	  <img src="/redesign/Top_Header.gif" alt="">
	  <img src="/redesign/ComicNav_Next.gif" alt="Next">
	  <img src="/images/logo.png" alt="Site">
	  <img src="/comics/strip/oots1347_a1b2c3.png" alt="Lowball Offer">
	  <img src="/Banners/Banner_Store.png" alt="">
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/comics/oots1347.html",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NamePageImages {
		t.Fatalf("extractor = %q, want %q", r.Name, extract.NamePageImages)
	}

	got := imageSources(r.HTML)
	if len(got) != 1 {
		t.Fatalf("kept %d images, want exactly the comic: %v", len(got), got)
	}
	if !strings.Contains(got[0], "oots1347_a1b2c3.png") {
		t.Errorf("kept %q, want the comic", got[0])
	}
	for _, chrome := range []string{"ComicNav", "logo", "Banner", "Top_Header"} {
		if strings.Contains(r.HTML, chrome) {
			t.Errorf("the body contains chrome image %q:\n%s", chrome, r.HTML)
		}
	}
}

// A multi-panel strip is several images under one slug, and all of them are the
// article.
func TestPageImagesKeepsEveryPanel(t *testing.T) {
	const page = `<html><body>
	  <img src="https://cdn.example.com/default/logo.png" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/1.png" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/2.jpg" alt="">
	  <img src="https://cdn.example.com/comics/design_hell/3.jpg" alt="">
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/comics/design_hell",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if got := imageSources(r.HTML); len(got) != 3 {
		t.Errorf("kept %d panels, want 3: %v", len(got), got)
	}
}

// A site whose articles are numbered — /482, /1347 — cannot have an image URL
// carrying the article's slug, so the slug signal can never fire there. What such a
// site does instead is name the file after the strip.
func TestPageImagesSelectsByTitle(t *testing.T) {
	const page = `<html>
	<head><title>Daily Strip: Deadline Season</title></head>
	<body>
	  <img src="/s/daily-strip.png" alt="Daily Strip">
	  <img src="/s/nav-next.gif" alt="Next">
	  <img src="//images.example.com/strips/deadline_season.png" alt="Deadline Season">
	  <img src="/s/banner-store.png" alt="">
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/482",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NamePageImages {
		t.Fatalf("extractor = %q, want %q", r.Name, extract.NamePageImages)
	}

	got := imageSources(r.HTML)
	if len(got) != 1 {
		t.Fatalf("kept %d images, want exactly the strip: %v", len(got), got)
	}
	if !strings.Contains(got[0], "deadline_season.png") {
		t.Errorf("kept %q, want the strip", got[0])
	}

	// The title's first segment is the site's name, and the site's logo is named
	// after it on every page there is. Matching that would put a logo in an
	// article on every numbered page of every site that titles pages this way.
	if strings.Contains(r.HTML, "daily-strip.png") {
		t.Errorf("the site's logo was taken for the article's content:\n%s", r.HTML)
	}
}

// The title match is exact, unlike the slug match, because a title is not part of
// an address: a substring of it is a coincidence rather than a naming convention.
func TestPageImagesTitleMatchIsExact(t *testing.T) {
	const page = `<html>
	<head><title>Deadline Season</title></head>
	<body><img src="/images/deadline.png" alt="A deadline"></body></html>`

	if _, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/482",
	}); err == nil {
		t.Error("an image named after part of the title was accepted as the article's content")
	}
}

// The hover text is where a comic keeps its joke, and a title attribute is not
// where an archive should keep it: the sanitizer's allowlist matches that attribute
// against a pattern rejecting quotation and question marks, so the punchline would
// survive on one strip and vanish on the next.
func TestPageImagesKeepsTheHoverTextAsACaption(t *testing.T) {
	const page = `<html>
	<head><title>Deadline Season</title></head>
	<body><img src="/strips/deadline_season.png" alt="Deadline Season"
	     title="&quot;Told you it would be fine?&quot; &quot;Yes.&quot;"></body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/482",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	for _, want := range []string{"<figcaption>", "Told you it would be fine?"} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("the stored body is missing %q:\n%s", want, r.HTML)
		}
	}
	// Search reads the text, so the joke has to be in both renderings.
	if !strings.Contains(r.Text, "Told you it would be fine?") {
		t.Errorf("the hover text is not in the extracted text: %q", r.Text)
	}
}

// The regression that archived every strip on a numbered site as its own footer.
//
// A comic page's footer carries images of its own — a thumbnail, a banner — so a
// text extraction that returned the footer and none of the comic satisfied a check
// for "the body has an image" and was left in place. The question worth asking is
// whether the body holds one of the images that name the article.
func TestAThinBodyHoldingOnlyChromeImagesLosesToTheStrip(t *testing.T) {
	const page = `<html>
	<head><title>Daily Strip: Deadline Season</title></head>
	<body>
	  <div id="comic"><img src="/strips/deadline_season.png" alt="Deadline Season"></div>
	  <div id="bottom">
	    <p><img src="/s/selected-strips.png" alt="Selected Strips"></p>
	    <p>Strips I enjoy: One Panel Only, Two Panels At Most, Three Panels On A Good
	    Day, Four Panels Minimum, Five Minutes To Deadline, Six Impossible Things.
	    This site is best viewed with a browser nobody has shipped since 1998.</p>
	  </div>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/482",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if !strings.Contains(r.HTML, "deadline_season.png") {
		t.Errorf("the strip is not in the stored body, so the page was archived as its footer:\n%s",
			r.HTML)
	}
	if strings.Contains(r.Text, "Strips I enjoy") {
		t.Errorf("the footer was stored as the article:\n%s", r.Text)
	}
}

// The rung must not fire when there is nothing to match, or it would replace
// real articles with whatever picture happened to be on the page.
func TestPageImagesDoesNotFireWithoutASlugMatch(t *testing.T) {
	const page = `<html><body>
	  <img src="/images/hero.png" alt="">
	  <img src="/images/author-portrait.jpg" alt="">
	</body></html>`

	_, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2026/some-article",
	})
	if err == nil {
		t.Error("extraction succeeded on a page with no slug-matching image, so unrelated pictures can be stored as articles")
	}
}

// A real article that happens to be short must keep its text.
func TestAShortArticleWithItsOwnImageIsNotReplaced(t *testing.T) {
	page := `<html><body><article><p>` +
		strings.Repeat("This is a short but genuine post about the subject at hand. ", 6) +
		`</p><img src="/2026/short-post/figure.png" alt="A figure"></article></body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2026/short-post",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name == extract.NamePageImages {
		t.Errorf("a short article carrying its own image was replaced by the image rung:\n%s", r.HTML)
	}
	if !strings.Contains(r.Text, "genuine post") {
		t.Errorf("the article's text was lost:\n%s", r.Text)
	}
}

// A domain rule is a human saying "the body is here". On an image-first page
// that body is a picture and a caption, far under the 200-character floor — so
// applying the floor rejects the one element the operator explicitly pointed at,
// and a comic ends up neither extracted automatically nor rescuable by hand.
func TestADomainRuleMayPointAtAnImage(t *testing.T) {
	const page = `<html><body>
	  <div id="chrome"><p>Navigation and a great deal of other text that goes on for
	     a while so that a text extractor has something it prefers to find, which is
	     exactly the situation a domain rule exists to correct on a page like this
	     one, where the real content is a single picture.</p></div>
	  <div id="strip"><img src="/comic/unrelated-name.png" alt="Today"></div>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://comics.example.com/comic/some-strip",
		Rule:    &extract.Rule{ContentSelector: "#strip"},
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NameDomainRule {
		t.Errorf("extractor = %q, want %q — the rule's selection was rejected for being short",
			r.Name, extract.NameDomainRule)
	}
	if !strings.Contains(r.HTML, "unrelated-name.png") {
		t.Errorf("the selected image is not in the body:\n%s", r.HTML)
	}
}

// The floor still applies when there is no image: a rule pointing at a paywall
// stub should still fall through rather than storing two sentences as an article.
func TestADomainRuleStillNeedsTextWhenThereIsNoImage(t *testing.T) {
	const page = `<html><body>
	  <div id="stub"><p>Subscribe to read.</p></div>
	  <div id="rest"><p>Some other text on the page entirely.</p></div>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2026/paywalled",
		Rule:    &extract.Rule{ContentSelector: "#stub"},
	})
	if err == nil && r.Name == extract.NameDomainRule {
		t.Errorf("a two-sentence imageless selection was accepted as an article:\n%s", r.HTML)
	}
}

// A short slug is evidence when it names the image's own folder, which is how the
// site that motivated this actually files its strips: /2020/err/171-err.png. A
// whole-file-name rule reached one strip of ten here, because only one happened to
// be named exactly after its article.
func TestAShortSlugMatchesItsFolder(t *testing.T) {
	const page = `<html><head><title>Err | Monkeyuser</title></head><body>
	  <header><img src="/images/logo.png" alt="Site"></header>
	  <div class="comic"><img src="/2020/err/171-err.png" alt="err" title="Off by one, again"></div>
	  <aside><img src="/assets/err-sprite-sheet.png" alt="chrome"></aside>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://www.monkeyuser.com/2020/err",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NamePageImages {
		t.Fatalf("extractor = %q, want %q", r.Name, extract.NamePageImages)
	}
	if !strings.Contains(r.HTML, "171-err.png") {
		t.Errorf("the strip, filed under a folder named after the article, is not in the body:\n%s", r.HTML)
	}
	// The counterweight, and the reason this is a component match and not a
	// substring one: "err" occurs in both of these too.
	if strings.Contains(r.HTML, "sprite-sheet") {
		t.Errorf("an asset that merely contains the slug was taken as content:\n%s", r.HTML)
	}
	if strings.Contains(r.HTML, "logo.png") {
		t.Errorf("the site logo was taken as content:\n%s", r.HTML)
	}
	if !strings.Contains(r.HTML, "Off by one") {
		t.Errorf("the hover text did not survive as a caption:\n%s", r.HTML)
	}
}

// A slug shorter than the substring floor is still evidence when it is the image's
// whole file name. The rung exists for image-only pages, and a webcomic titled
// "10x" is exactly such a page.
func TestAShortSlugMatchesAWholeFileName(t *testing.T) {
	const page = `<html><head><title>10x | Monkeyuser</title></head><body>
	  <header><img src="/images/logo.png" alt="Site"></header>
	  <div class="comic"><img src="/2025/10x/10x.png" alt="10x" title="Wouldn't a backup of a backup be redundant?"></div>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://www.monkeyuser.com/2025/10x",
	})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if r.Name != extract.NamePageImages {
		t.Fatalf("extractor = %q, want %q", r.Name, extract.NamePageImages)
	}
	if !strings.Contains(r.HTML, "10x.png") {
		t.Errorf("the strip is not in the body:\n%s", r.HTML)
	}
	if strings.Contains(r.HTML, "logo.png") {
		t.Errorf("the site logo was taken as content:\n%s", r.HTML)
	}
	// The hover text is where the joke is, and it is the reason this rung has to
	// be the one that reaches these pages rather than a domain rule.
	if !strings.Contains(r.HTML, "redundant") {
		t.Errorf("the hover text did not survive as a caption:\n%s", r.HTML)
	}
}

// The other half of relaxing the floor: a short slug must not match by appearing
// somewhere inside an unrelated URL, which is the coincidence the floor was there
// to prevent. Only the whole file name counts.
func TestAShortSlugDoesNotMatchASubstring(t *testing.T) {
	const page = `<html><head><title>Untitled</title></head><body>
	  <div><img src="/assets/10x-sprite-sheet.png" alt="chrome"></div>
	  <div><img src="/banners/ad-10x-wide.png" alt="ad"></div>
	</body></html>`

	r, err := extract.New().Extract(extract.Input{
		RawHTML: []byte(page),
		URL:     "https://example.com/2025/10x",
	})
	if err == nil && r.Name == extract.NamePageImages {
		t.Errorf("a short slug pulled in images that merely contain it:\n%s", r.HTML)
	}
}
