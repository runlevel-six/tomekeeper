package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

// exportDeadline is how long the export route may take to write its response.
//
// The server's write timeout is 30 seconds, which is right for a page and wrong for
// a download: an export of the maintainer's archive takes three seconds per 385
// articles, so the timeout arrives somewhere north of eight thousand — a number an
// archive reaches quietly, years in, at which point the download would begin
// failing with no explanation. Ten minutes is far past any plausible archive and
// still bounded, so a stalled client cannot hold a connection open forever.
//
// Extended for this route alone rather than by raising the server's timeout. One
// slow response is not a reason to let every response be slow.
const exportDeadline = 10 * time.Minute

// handleExport streams the archive as a file.
//
// The counterpart to the import upload, and it streams rather than buffering: an
// export is the one response whose size grows with the archive, and holding a decade
// of articles in memory to measure them before sending is how a feature works in
// testing and fails on the machine that needed it.
//
// Streaming costs one thing worth stating: the status is sent before the work is
// done, so a failure partway through cannot be reported as an error — the download
// simply ends early. That is survivable here because the file is JSON and the
// importer refuses a truncated one outright, saying the file ends before its last
// record. A cut-off download is therefore caught on the way back in rather than
// restored as a partial archive.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	// A count first, so the log line afterwards can be compared against it and so an
	// empty archive is not silently offered as a download.
	total, err := s.store.CountExportable(r.Context(), userID)
	if err != nil {
		s.log.Error("counting the archive for export failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(exportDeadline)); err != nil {
		// Not fatal: the handler still works, it simply inherits the server's
		// timeout, which is enough for any archive small enough to notice.
		s.log.Warn("could not extend the write deadline for an export", "error", err)
	}

	filename := "tomekeeper-" + time.Now().UTC().Format("2006-01-02") + ".json"

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// Nothing about this response is cacheable: it is a snapshot of an archive that
	// changes, and a stale one masquerading as a backup is worse than none.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	result, err := exchange.Export(r.Context(), s.store, userID, w)
	if err != nil {
		// The response is already in flight, so this cannot become a 500. Logged with
		// how far it got, and the truncated file will be refused by the importer.
		s.log.Error("the export failed partway through; the download is truncated",
			"error", err, "articles_written", result.Articles, "articles_expected", total)
		return
	}

	s.log.Info("exported the archive",
		"articles", result.Articles, "bodies", result.Bodies,
		"tags", result.Tags, "highlights", result.Highlights, "assets", result.Assets)
}

// exportSummary is what the settings page says the download will contain.
type exportSummary struct {
	Articles int64

	// Filename is what the browser will save it as, shown so that somebody looking
	// for it afterwards knows what they are looking for.
	Filename string
}

func (s *Server) exportSummaryFor(r *http.Request) *exportSummary {
	total, err := s.store.CountExportable(r.Context(), signedInUser(r))
	if err != nil {
		// A missing count costs the sentence, not the control.
		s.log.Warn("counting the archive for the export control failed", "error", err)
		return nil
	}

	return &exportSummary{
		Articles: total,
		Filename: fmt.Sprintf("tomekeeper-%s.json", time.Now().UTC().Format("2006-01-02")),
	}
}
