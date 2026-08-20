// Package render fetches a page through a real browser, for the small number of
// sites that serve an empty shell and build the article in JavaScript.
//
// # Why this is not part of extraction
//
// The obvious place for this is another rung on the extraction ladder, and that is
// where the design originally put it. It belongs at *fetch* time instead, and the
// reason is a property worth more than the tidiness: extraction touches no network.
// A body is a derived view over a stored page, so `tome reextract` can improve every
// article in the archive without asking any origin server for anything. A rung that
// rendered would make re-extraction re-fetch — thousands of requests to other
// people's sites every time the extractor improved.
//
// So a flagged domain is *fetched* through the browser, and what gets stored as the
// raw page is the rendered DOM. Extraction then runs over it unchanged, offline,
// exactly as it does for every other article, and future improvements reach these
// articles for free like any other.
//
// # Why the browser is somewhere else
//
// This package speaks CDP to a browser it does not start. The alternative — bundling
// Chrome — would end the single static binary: the image is distroless with
// CGO_ENABLED=0 and the binary is 34MB, against 300MB+ for a Chrome base. Keeping the
// browser in its own deployment also puts its memory where it can be limited without
// limiting the worker, which is what makes an out-of-memory render somebody else's
// crash rather than the archive's.
//
// # Politeness, and the part that cannot be fixed
//
// A browser is not a polite HTTP client. Loading one page fires dozens of requests at
// hosts nobody chose — advertising, analytics, fonts, third-party script — none of
// which pass through this archive's rate limiter, its robots.txt cache, or its honest
// User-Agent. Three mitigations are applied here, and one problem is left standing:
//
//   - Images, media and fonts are blocked outright. The archive fetches images itself,
//     afterwards, through the polite client — so loading them twice would be both
//     rude and pointless. This is also most of the memory saving.
//   - The User-Agent is this archive's, not Chrome's, so a site that wants to know who
//     is asking gets a truthful answer with a contact URL in it.
//   - The caller is expected to have cleared the document URL against robots.txt
//     already, through the same client and cache the ordinary fetch path uses. This
//     package does not check, because a second implementation of that rule is how the
//     two come to disagree.
//
// What remains is that third-party JavaScript executes. That is what rendering *is*,
// and no amount of request filtering changes it: scripts on the page will run, and
// some of them will report back. This is a deliberate, documented exception to the
// politeness principle, confined to the domains an operator has flagged by hand.
package render

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// DefaultTimeout bounds one render.
//
// Generous by the standards of the rest of this application, because the thing being
// waited for is a site's own JavaScript finishing, and that is not this archive's
// budget to set. Short enough that a hung browser costs one article rather than a
// queue: the render queue is deliberately narrow (see the worker's configuration), so
// a wedged page holds a slot for this long and nothing else.
const DefaultTimeout = 45 * time.Second

// blockedTypes are the subresources a render has no use for.
//
// Extraction reads the DOM; it never looks at a pixel. So every one of these is a
// request to somebody's server for bytes that would be parsed by nobody — and images
// in particular are fetched again minutes later by the asset pipeline, politely,
// rate-limited and honestly identified. Blocking them is the difference between one
// request per article and dozens, and it is most of the memory saving too.
//
// Blocked by *resource type* rather than by URL pattern, which costs an interception
// handler and is worth it: Chrome's blocked-URL list matches on the URL, and a great
// many images are served from a CDN path with no extension and a query string. Type
// matching is what makes "images are not loaded" true rather than mostly true.
//
// Stylesheets are deliberately allowed. They do not change the DOM, so extraction has
// no use for them either — but a site's own script sometimes waits on one before it
// builds the page, and a render that blocks CSS to save a request and then extracts
// nothing has saved nothing.
var blockedTypes = map[network.ResourceType]bool{
	network.ResourceTypeImage: true,
	network.ResourceTypeMedia: true,
	network.ResourceTypeFont:  true,
}

// ErrUnavailable means no browser could be reached.
//
// Distinguished from a page that failed to render because the remedies are opposite:
// this one is "the render deployment is scaled to zero, or its pod is gone", which is
// an operator's business and is the *expected* state on an installation that has never
// wanted rendering. A page that failed on its own merits is the archive's business and
// belongs in the attention queue.
var ErrUnavailable = errors.New("no browser is available")

// Renderer fetches pages through a remote browser.
//
// Safe for concurrent use, though the worker deliberately does not use it that way:
// each render is an independent browser context and the memory cost is per render.
type Renderer struct {
	// wsURL is the browser's CDP endpoint, as advertised at /json/version.
	wsURL string

	// userAgent replaces Chrome's own, so the sites being rendered are told who is
	// really asking.
	userAgent string

	timeout time.Duration
}

// Options configure a Renderer.
type Options struct {
	// WebSocketURL is the browser's CDP endpoint. Required; an empty one yields a nil
	// Renderer rather than an error, because "no browser configured" is a normal
	// deployment and not a misconfiguration.
	WebSocketURL string

	// UserAgent is sent instead of the browser's. Required, for the same reason the
	// outbound HTTP client insists on one: a fetcher that will not say who it is has
	// no business rendering anybody's pages.
	UserAgent string

	// Timeout bounds one render. Zero means DefaultTimeout.
	Timeout time.Duration
}

// New returns a Renderer, or nil when no browser is configured.
//
// Nil is a supported value everywhere a Renderer is used: an installation that has
// never flagged a domain has no reason to run a browser, and the fetch path treats a
// nil Renderer as "rendering is unavailable" rather than as an error to report.
func New(opts Options) (*Renderer, error) {
	if opts.WebSocketURL == "" {
		return nil, nil
	}
	if opts.UserAgent == "" {
		return nil, errors.New("render: a user agent is required")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Renderer{wsURL: opts.WebSocketURL, userAgent: opts.UserAgent, timeout: timeout}, nil
}

// Page is one rendered document.
type Page struct {
	// HTML is the serialized DOM after the page's scripts have run.
	HTML string

	// Blocked counts the subresource requests this render refused.
	//
	// Reported rather than merely done, because it is the number that says whether the
	// politeness mitigation is working: a render of a modern news page that blocks
	// nothing is a render that made thirty requests nobody asked for. It goes in the
	// worker's log line.
	Blocked int

	// Requests counts the subresource requests that were allowed through, which is the
	// honest other half — the traffic this archive caused at hosts it never chose.
	Requests int
}

// Render loads url in the browser and returns the DOM once its scripts have run.
//
// The returned HTML is the serialized document, which is what the extraction ladder
// then treats as the page — so from extraction's point of view a rendered article is
// indistinguishable from a fetched one, which is the point.
//
// The caller must have checked robots.txt for this URL already. See the package
// comment for why that check does not live here.
func (r *Renderer) Render(ctx context.Context, url string) (Page, error) {
	if r == nil {
		return Page{}, ErrUnavailable
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// A remote allocator: this process never starts, supervises or reaps a browser. If
	// the endpoint is unreachable the failure surfaces on the first action below.
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, r.wsURL)
	defer cancelAlloc()

	// A fresh browser context per render, so one page's state — cookies, storage,
	// service workers — cannot reach the next. That matters more here than usual: the
	// pages being rendered are exactly the ones running the most third-party script.
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// Connect before doing anything, so that "no browser" and "bad page" are told
	// apart by *when* the failure happened rather than by inspecting an error's text.
	// A bare Run establishes the target and nothing else; chromedp offers no typed
	// error for a failed dial, and guessing at its wording is how this returned the
	// wrong classification the first time it was tested.
	if err := chromedp.Run(browserCtx); err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	counts := blockSubresources(browserCtx)

	var html string
	err := chromedp.Run(browserCtx,
		fetch.Enable(),
		emulation.SetUserAgentOverride(r.userAgent),
		chromedp.Navigate(url),
		// The document, after scripts have run, exactly as the browser holds it.
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		// Past the connection, so this is the page's failure rather than the browser's:
		// a script that never finished, a navigation that never committed, a document
		// that would not serialize. The article gets recorded as failed and lands in the
		// attention queue, which is where a site needing a rule belongs.
		return Page{}, fmt.Errorf("rendering %s: %w", url, err)
	}
	if html == "" {
		return Page{}, fmt.Errorf("rendering %s: the browser returned an empty document", url)
	}

	// Serialized without the doctype, since OuterHTML gives the element rather than the
	// document. Extraction parses a fragment happily, but a stored page that opens in a
	// browser should look like a page.
	return Page{
		HTML:     "<!DOCTYPE html>\n" + html,
		Blocked:  int(counts.blocked.Load()),
		Requests: int(counts.allowed.Load()),
	}, nil
}

// interceptCounts tallies what one render's interception handler did.
//
// Atomic because the handler answers each request in its own goroutine, and the
// counters are read after Run returns rather than during — a race the detector would
// find on the first CI run otherwise.
type interceptCounts struct {
	blocked atomic.Int64
	allowed atomic.Int64
}

// blockSubresources answers every intercepted request: refusing the types in
// blockedTypes and letting everything else through.
//
// Every paused request must be answered or the render stalls until its deadline, so
// the default is to continue rather than to block — a resource type nobody thought
// about loads, which is the failure that costs a request rather than the article.
//
// Each answer goes in its own goroutine because the handler runs on the event loop
// that would carry the reply, and answering inline deadlocks. The executor has to be
// built from the target rather than reusing the action context, which is the part of
// this idiom that is easy to get wrong and silently hangs.
func blockSubresources(ctx context.Context) *interceptCounts {
	counts := &interceptCounts{}

	chromedp.ListenTarget(ctx, func(ev any) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}

		go func() {
			c := chromedp.FromContext(ctx)
			if c == nil || c.Target == nil {
				return
			}
			exec := cdp.WithExecutor(ctx, c.Target)

			if blockedTypes[paused.ResourceType] {
				// BlockedByClient rather than a silent abort: a site that notices its
				// images were refused is being told the truth about what happened.
				counts.blocked.Add(1)
				_ = fetch.FailRequest(paused.RequestID, network.ErrorReasonBlockedByClient).Do(exec)
				return
			}
			counts.allowed.Add(1)
			_ = fetch.ContinueRequest(paused.RequestID).Do(exec)
		}()
	})

	return counts
}
