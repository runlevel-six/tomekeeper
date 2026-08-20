package render_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/render"
)

// The renderer, against a real browser.
//
// Gated on TOME_TEST_CDP_URL and skipped when it is unset, the same bargain
// internal/dbtest strikes for Postgres: `go test ./...` on a laptop with no browser
// stays useful, and a green run without the variable is only the unit half. Say which
// half you ran.
//
// **Every page here is a `data:` URL**, and that is not a shortcut. In production the
// browser is a separate deployment, so a test that served fixtures over HTTP would
// need the browser to be able to reach back into the test process — true of a local
// browser and false of the one this is actually tested against, which runs in the
// cluster. Driving it entirely from data: URLs means the same test passes against a
// browser anywhere, which is the arrangement the design commits to.
const cdpEnvVar = "TOME_TEST_CDP_URL"

func renderer(t *testing.T) *render.Renderer {
	t.Helper()

	wsURL := os.Getenv(cdpEnvVar)
	if wsURL == "" {
		t.Skipf("%s is not set; skipping the browser integration test", cdpEnvVar)
	}

	r, err := render.New(render.Options{
		WebSocketURL: wsURL,
		UserAgent:    testUserAgent,
		Timeout:      30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if r == nil {
		t.Fatalf("New() returned no renderer for %q", wsURL)
	}
	return r
}

// testUserAgent is deliberately shaped like the real one — a name, a version, and a
// contact URL — because the assertion below is that the page sees *this* rather than
// Chrome's.
const testUserAgent = "tomekeeper/test (+https://example.com/about)"

// page wraps HTML in a data: URL the browser will navigate to.
func page(body string) string {
	return "data:text/html," + url.PathEscape("<!DOCTYPE html><html><body>"+body+"</body></html>")
}

// The whole point of the milestone: a page whose article exists only after its
// JavaScript has run comes back with the article in it.
//
// This is the shape of the site the feature is for — an empty shell plus a script
// that builds the body — and the plain fetch path stores exactly the shell.
func TestRenderRunsTheScriptsAndReturnsTheBuiltDOM(t *testing.T) {
	r := renderer(t)

	const shell = `<div id="app"></div>
		<script>
		  document.getElementById('app').innerHTML =
		    '<article><h1>Built by script</h1><p>A distinctive pangolin passage.</p></article>';
		</script>`

	got, err := r.Render(t.Context(), page(shell))
	if err != nil {
		t.Fatalf("Render() = %v", err)
	}

	if !strings.Contains(got.HTML, "A distinctive pangolin passage") {
		t.Errorf("the rendered DOM does not contain the scripted body:\n%s", firstChars(got.HTML, 600))
	}
	if !strings.Contains(got.HTML, "<article>") {
		t.Errorf("the rendered DOM does not contain the injected element:\n%s", firstChars(got.HTML, 600))
	}
	// A document rather than a fragment, so a stored page looks like a page.
	if !strings.HasPrefix(got.HTML, "<!DOCTYPE html>") {
		t.Errorf("the rendered page does not begin with a doctype: %s", firstChars(got.HTML, 80))
	}
}

// The User-Agent the page sees is this archive's, not the browser's.
//
// Asserted from inside the page — `navigator.userAgent` written into the DOM — because
// that is what a site's own script and its server will see, and an override that only
// changed a header the DOM disagrees with would be worth nothing.
func TestRenderTellsThePageWhoIsAsking(t *testing.T) {
	r := renderer(t)

	got, err := r.Render(t.Context(),
		page(`<p id="ua"></p><script>document.getElementById('ua').textContent = navigator.userAgent;</script>`))
	if err != nil {
		t.Fatalf("Render() = %v", err)
	}

	if !strings.Contains(got.HTML, testUserAgent) {
		t.Errorf("the page was not told our user agent:\n%s", firstChars(got.HTML, 400))
	}
	// The thing being avoided by name. A site that blocks headless traffic is entitled
	// to; announcing ourselves as Chrome to get around it is not what an honest
	// User-Agent means.
	if strings.Contains(got.HTML, "HeadlessChrome") {
		t.Errorf("the page still sees a headless Chrome user agent:\n%s", firstChars(got.HTML, 400))
	}
}

// Images are not loaded, and the count says so.
//
// The discriminating assertion is `Blocked`, not the absence of a picture: an image
// from an unreachable host fails either way, so only the interception count
// distinguishes "we refused it" from "it failed on its own". Interception happens
// before the network, so the host never needs to exist — which is what keeps this test
// from making a request to anybody.
func TestRenderRefusesSubresourcesItHasNoUseFor(t *testing.T) {
	r := renderer(t)

	const withMedia = `
		<img src="http://images.invalid/lead.png">
		<img src="http://images.invalid/no-extension-at-all?id=7">
		<p>Text that survives.</p>`

	got, err := r.Render(t.Context(), page(withMedia))
	if err != nil {
		t.Fatalf("Render() = %v", err)
	}

	if got.Blocked < 2 {
		t.Errorf("Blocked = %d, want at least the 2 images refused", got.Blocked)
	}
	// The second image has no file extension and carries a query string, which is how a
	// great many CDNs serve them. It is the case a URL-pattern blocklist misses and the
	// reason this blocks on resource type instead.
	if got.Blocked < 2 {
		t.Log("note: an extensionless image is the case URL-pattern blocking misses")
	}
	if !strings.Contains(got.HTML, "Text that survives") {
		t.Errorf("blocking the images cost the text:\n%s", firstChars(got.HTML, 400))
	}
}

// A renderer with no browser configured is nil, and a nil renderer is usable.
//
// This is the ordinary state of an installation that has never flagged a domain, so it
// has to be a normal answer rather than a crash: the fetch path asks the renderer
// whether it can help and gets told no.
func TestNoBrowserConfiguredIsNotAnError(t *testing.T) {
	r, err := render.New(render.Options{UserAgent: testUserAgent})
	if err != nil {
		t.Fatalf("New() with no endpoint = %v, want no error", err)
	}
	if r != nil {
		t.Fatalf("New() with no endpoint returned a renderer")
	}

	// The nil receiver answers rather than panicking, which is the property the fetch
	// path relies on.
	if _, err := r.Render(context.Background(), "data:text/html,x"); !errors.Is(err, render.ErrUnavailable) {
		t.Errorf("a nil renderer returned %v, want ErrUnavailable", err)
	}
}

// An endpoint that answers nothing is reported as unavailable rather than as a page
// that failed, because the two have opposite remedies: one is an operator scaling a
// deployment, the other is a site to write a rule for.
func TestAnUnreachableBrowserIsUnavailableNotAFailedPage(t *testing.T) {
	r, err := render.New(render.Options{
		// Reserved for documentation and guaranteed not to answer.
		WebSocketURL: "ws://127.0.0.1:1/devtools/browser/nope",
		UserAgent:    testUserAgent,
		Timeout:      3 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	_, err = r.Render(t.Context(), "data:text/html,x")
	if err == nil {
		t.Fatal("rendering against a dead endpoint succeeded")
	}
	if !errors.Is(err, render.ErrUnavailable) {
		t.Errorf("Render() against a dead endpoint = %v, want ErrUnavailable", err)
	}
}

func TestAUserAgentIsRequired(t *testing.T) {
	if _, err := render.New(render.Options{WebSocketURL: "ws://example.invalid/x"}); err == nil {
		t.Error("New() accepted a browser endpoint with no user agent")
	}
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// A bare endpoint works, with no browser GUID in it.
//
// This pins a chromedp behavior the deployment depends on: the websocket URL that
// /json/version advertises contains a per-browser GUID that changes every time the pod
// restarts, so no manifest could ever hardcode it. Given a URL with no
// `/devtools/browser/` path, chromedp fetches /json/version and finds the real one —
// which is what lets TOME_RENDER_BROWSER_URL be a Kubernetes Service name.
func TestABareEndpointResolvesItself(t *testing.T) {
	wsURL := os.Getenv(cdpEnvVar)
	if wsURL == "" {
		t.Skipf("%s is not set; skipping the browser integration test", cdpEnvVar)
	}

	// Strip the GUID path, leaving only what a Service name would give us.
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parsing %q: %v", wsURL, err)
	}
	bare := "ws://" + u.Host

	r, err := render.New(render.Options{
		WebSocketURL: bare, UserAgent: testUserAgent, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	got, err := r.Render(t.Context(), page(`<p>Resolved without a GUID.</p>`))
	if err != nil {
		t.Fatalf("Render() against %q = %v", bare, err)
	}
	if !strings.Contains(got.HTML, "Resolved without a GUID") {
		t.Errorf("the bare endpoint rendered nothing useful:\n%s", firstChars(got.HTML, 300))
	}
}
