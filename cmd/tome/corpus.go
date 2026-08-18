package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
)

// corpusCmd captures real pages into the private extraction corpus.
//
// The corpus is the regression suite for every extraction change, and it has been
// empty of real pages since the mechanism was built — which is why a site that
// splits its article across three blocks got as far as somebody noticing it in the
// reader. A synthetic fixture only ever exercises the structures its author thought
// of; a real page exercises the ones nobody did.
//
// The pages are third-party content and do not belong in this repository, so they
// live in the directory TOME_TEST_CORPUS_DIR names. This command exists to make
// adding one cost a minute rather than an afternoon: it fetches the page, saves it
// exactly as fetched, runs the current extractor over it, and writes a starter
// expectations file with the parts only a person can decide left blank.
func corpusCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		corpusUsage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "add":
		return corpusAdd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tome corpus: unknown action %q\n\n", args[0])
		corpusUsage(stderr)
		return exitUsage
	}
}

func corpusUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: tome corpus add [--name NAME] <url>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Saves a page into the private extraction corpus named by TOME_TEST_CORPUS_DIR,")
	fmt.Fprintln(w, "along with a starter expectations file to edit.")
}

func corpusAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("corpus add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "file stem to use (default: derived from the URL)")
	fs.Usage = func() { corpusUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		corpusUsage(stderr)
		return exitUsage
	}
	pageURL := fs.Arg(0)

	dir := os.Getenv(corpusDirEnv)
	if dir == "" {
		fmt.Fprintf(stderr, "tome corpus: %s is not set.\n", corpusDirEnv)
		fmt.Fprintln(stderr, "It names a directory outside this repository, because the pages are")
		fmt.Fprintln(stderr, "third-party content. Point it somewhere and run this again.")
		return exitUsage
	}
	// The corpus directory is the operator's own environment variable and the stem
	// is reduced to [a-z0-9-] before it is used, so neither can escape anywhere —
	// see safeStem below, which is what makes the file operations here safe rather
	// than merely unattacked. gosec's taint analysis cannot see that, hence the
	// annotations.
	info, err := os.Stat(dir) //nolint:gosec // operator-supplied directory; see safeStem
	if err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "tome corpus: %s is %q, which is not a directory\n", corpusDirEnv, dir)
		return exitUsage
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	ctx, stop := signalContext()
	defer stop()

	stem := *name
	if stem == "" {
		stem = corpusStem(pageURL)
	}
	if stem == "" {
		fmt.Fprintln(stderr, "tome corpus: could not derive a name from that URL; pass --name")
		return exitUsage
	}
	if !safeStem(stem) {
		fmt.Fprintf(stderr, "tome corpus: %q is not a usable file name; letters, digits and dashes only\n", stem)
		return exitUsage
	}

	htmlPath := filepath.Join(dir, stem+".html")
	wantPath := filepath.Join(dir, stem+".want")
	for _, path := range []string{htmlPath, wantPath} {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // stem is [a-z0-9-]; see safeStem
			fmt.Fprintf(stderr, "tome corpus: %s already exists; pass --name for a different stem\n", path)
			return exitUsage
		}
	}

	// Fetched with the archive's own client, which means its user agent, its rate
	// limits, and its robots.txt handling. A corpus page is an article fetch like
	// any other, and a site that has asked not to be fetched has asked this too.
	client := newHTTPClient(cfg)
	resp, err := client.Do(ctx, httpclient.Request{URL: pageURL})
	if err != nil {
		fmt.Fprintf(stderr, "tome corpus: fetching %s: %v\n", pageURL, err)
		return exitFailure
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		fmt.Fprintf(stderr, "tome corpus: %s answered HTTP %d\n", pageURL, resp.StatusCode)
		return exitFailure
	}

	raw, err := httpclient.ReadBody(resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "tome corpus: reading %s: %v\n", pageURL, err)
		return exitFailure
	}

	if err := os.WriteFile(htmlPath, raw, 0o600); err != nil { //nolint:gosec // stem is [a-z0-9-]; see safeStem
		fmt.Fprintf(stderr, "tome corpus: writing %s: %v\n", htmlPath, err)
		return exitFailure
	}

	// What the current extractor makes of it, which is what the starter file
	// records. A case captured today asserts today's behavior; the value is that a
	// change tomorrow has to be looked at rather than discovered.
	result, extractErr := extract.New().Extract(extract.Input{RawHTML: raw, URL: pageURL})

	//nolint:gosec // stem is [a-z0-9-]; see safeStem
	if err := os.WriteFile(wantPath, []byte(starterWant(pageURL, result, extractErr)), 0o600); err != nil {
		fmt.Fprintf(stderr, "tome corpus: writing %s: %v\n", wantPath, err)
		return exitFailure
	}

	log.Debug("captured a corpus page", "url", pageURL, "bytes", len(raw))

	fmt.Fprintf(stdout, "saved %s (%d KB)\n", htmlPath, len(raw)/1024)
	if extractErr != nil {
		fmt.Fprintf(stdout, "\nExtraction currently FAILS on this page: %v\n", extractErr)
		fmt.Fprintln(stdout, "That is a good reason to have captured it. Edit the .want file to say what")
		fmt.Fprintln(stdout, "*should* come out, and the corpus will fail until it does.")
	} else {
		fmt.Fprintf(stdout, "extracted %d characters via %s\n", len(result.Text), result.Name)
	}

	fmt.Fprintf(stdout, "\nNow edit %s:\n", wantPath)
	fmt.Fprintln(stdout, "  - keep a few phrases from the middle and the end of the article")
	fmt.Fprintln(stdout, "  - add ! lines for navigation, bylines or promotional text that must not appear")
	fmt.Fprintln(stdout, "\nThe ! lines are the ones that catch regressions worth catching.")
	fmt.Fprintln(stdout, "Then: task test:corpus")
	return exitOK
}

// safeStem reports whether a file stem can only name a file inside the corpus
// directory.
//
// Letters, digits and dashes: no separators, no dots, and therefore no `..`. The
// derived stems are reduced to this by construction, but --name is whatever
// somebody typed, and "it happens to be safe today" is not the same claim as "it
// cannot be otherwise".
func safeStem(stem string) bool {
	if stem == "" {
		return false
	}
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// corpusDirEnv names the directory of real pages. It matches what the extraction
// tests read, deliberately: one variable, one meaning.
const corpusDirEnv = "TOME_TEST_CORPUS_DIR"

// starterWant is the expectations file a captured page begins with.
//
// Headers filled in from what actually happened, and the assertions left to a
// person — with the article's own opening and closing sentences quoted underneath as
// comments, because choosing a phrase to assert on is much easier when the text is
// in front of you than when it is in a browser tab.
func starterWant(pageURL string, result extract.Result, extractErr error) string {
	var b strings.Builder

	fmt.Fprintf(&b, "url: %s\n", pageURL)

	if extractErr != nil {
		b.WriteString("\n")
		b.WriteString("# Extraction currently fails on this page.\n")
		b.WriteString("#\n")
		b.WriteString("# Write what should come out, and the corpus will fail until it does. If the\n")
		b.WriteString("# right answer is that nothing should, add:  expect: none\n")
		return b.String()
	}

	fmt.Fprintf(&b, "extractor: %s\n", result.Name)
	// Deliberately below what was extracted rather than equal to it: an assertion
	// that breaks when a body grows by one character is an assertion nobody keeps.
	fmt.Fprintf(&b, "min_chars: %d\n", (len(result.Text)*9)/10)
	if images := countImagesIn(result.HTML); images > 0 {
		fmt.Fprintf(&b, "min_images: %d\n", images)
	}

	b.WriteString("\n")
	b.WriteString("# Replace these with phrases worth asserting: something from the middle of the\n")
	b.WriteString("# article and something from the end, so a truncated extraction fails here.\n")
	b.WriteString("# Then add ! lines for text that must NOT appear — navigation, related stories,\n")
	b.WriteString("# subscription prompts. Those are the ones that catch a regression.\n")
	b.WriteString("#\n")

	for _, line := range sampleLines(result.Text) {
		fmt.Fprintf(&b, "# %s\n", line)
	}

	return b.String()
}

// sampleLines quotes the opening and closing of an extracted body.
func sampleLines(text string) []string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}

	take := func(words []string) string {
		const max = 14
		if len(words) > max {
			words = words[:max]
		}
		return strings.Join(words, " ")
	}

	lines := []string{"opens: " + take(fields)}
	if len(fields) > 28 {
		lines = append(lines, "ends:  "+take(fields[len(fields)-14:]))
	}
	return lines
}

var corpusImgPattern = regexp.MustCompile(`(?i)<img[^>]*>`)

func countImagesIn(body string) int { return len(corpusImgPattern.FindAllString(body, -1)) }

// corpusStem derives a file stem from a URL: the host and the last path segment.
//
// Both, because a corpus of thirty pages from a dozen sites is much easier to read
// as `arstechnica-camp-miasma` than as `camp-miasma`, and because two sites can
// easily publish the same slug.
func corpusStem(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		// No host, so there is no site to name the file after — and nothing to fetch
		// either. Reported as "pass --name" rather than derived from whatever the
		// string happened to contain.
		return ""
	}

	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}

	slug := ""
	for _, segment := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if segment != "" {
			slug = segment
		}
	}
	slug = strings.TrimSuffix(slug, ".html")

	stem := strings.Trim(host+"-"+slug, "-")
	stem = corpusUnsafe.ReplaceAllString(strings.ToLower(stem), "-")
	stem = strings.Trim(corpusDashes.ReplaceAllString(stem, "-"), "-")

	const maxStem = 60
	if len(stem) > maxStem {
		stem = strings.Trim(stem[:maxStem], "-")
	}
	return stem
}

var (
	corpusUnsafe = regexp.MustCompile(`[^a-z0-9]+`)
	corpusDashes = regexp.MustCompile(`-{2,}`)
)
