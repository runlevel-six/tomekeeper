// Package httpclient is the single outbound HTTP client shared by every
// component that talks to the internet.
//
// At M1 it provides an honest User-Agent, bounded timeouts, and a response
// size cap. Per-host rate limiting and robots.txt arrive with M2, when the
// service starts fetching article pages rather than only the feeds a user
// explicitly subscribed to. The interface is settled now so that adding them
// does not mean touching every call site.
//
// Principle 2.6: be a polite network citizen. This is ethics and self-interest
// at once — impolite fetchers get their addresses banned, and then the archive
// stops growing.
package httpclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// MaxResponseBytes caps any single response body. A feed or article larger
// than this is a malfunction or an attack, not content worth archiving.
const MaxResponseBytes = 10 << 20 // 10MB

// Client is a configured HTTP client.
type Client struct {
	hc        *http.Client
	userAgent string
}

// UserAgent builds the outbound identification string.
//
// Contactable and honest, with no browser spoofing: an operator who wants this
// archiver to stop should be able to work out who to ask. contactURL may be
// empty, in which case it is simply omitted rather than faked.
func UserAgent(version, contactURL string) string {
	if contactURL == "" {
		return fmt.Sprintf("tomekeeper/%s", version)
	}
	return fmt.Sprintf("tomekeeper/%s (+%s)", version, contactURL)
}

// New returns a client that identifies itself with the given User-Agent.
func New(userAgent string) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		// Kept low deliberately: this is the per-host connection budget, and
		// the point of the fetcher is to be gentle with any single origin.
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	return &Client{
		hc: &http.Client{
			Transport: transport,
			// Total budget for a request including all redirects and the body.
			Timeout: 30 * time.Second,
		},
		userAgent: userAgent,
	}
}

// Get issues a GET request with the client's User-Agent and any extra headers,
// such as conditional-GET validators.
//
// The caller owns the response body and must close it.
func (c *Client) Get(ctx context.Context, url string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", url, err)
	}

	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	// Set last so a caller cannot accidentally override the honest identity.
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	return resp, nil
}

// ReadBody reads a response body up to MaxResponseBytes.
//
// It reads one byte past the cap so that hitting the limit is reported as an
// error rather than silently truncating, which would hand a half-downloaded
// document to a parser and produce a confusing failure much further along.
func ReadBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("response exceeds the %d byte limit", MaxResponseBytes)
	}
	return body, nil
}
