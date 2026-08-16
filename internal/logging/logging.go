// Package logging constructs the process-wide structured logger.
//
// Structured from the first line of code, not retrofitted. Operational
// questions this service will actually be asked — which feed is failing, which
// domain is rate-limiting us, how long extraction takes — are queries over
// fields, and they are only queries if the fields were there from the start.
package logging

import (
	"io"
	"log/slog"
)

// New returns a logger writing to w in the given format ("json" or "text") at
// the given minimum level. Unknown formats fall back to JSON; config.Load has
// already rejected them, so this is belt and braces rather than policy.
func New(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
