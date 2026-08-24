package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/backup"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// backupDeadline is how long the response may take to write.
//
// A backup is the one response whose size is the size of the archive, so it needs the
// same escape from the server's write timeout that the export does — and more of it,
// because it carries the images rather than the text.
const backupDeadline = 6 * time.Hour

// backupRunning guards against two backups at once.
//
// Not a lock: a second request is refused rather than queued. Two concurrent backups
// read the whole tree twice for no benefit, and the second one is nearly always
// somebody who did not see the first still running.
var backupRunning atomic.Bool

// handleBackup streams a complete backup to an administrator's browser.
//
// **Administrators only, and it is not the export.** `GET /export` is one reader's
// articles as JSON, scoped by what they can see and importable elsewhere. This is the
// household's bytes — every reader's articles and the raw pages nobody's account owns —
// so it is on the accounts page rather than beside the export in Settings, where the two
// would be conflated by anybody skimming.
//
// **Restoring stays a command.** A restore replaces every table and rewrites the tree,
// so the writers have to be stopped first, and nothing reachable from inside the
// running application can arrange for the application to be stopped. So this route
// exists and its counterpart deliberately does not.
//
// The response streams straight from the archive with nothing buffered, which is what
// keeps the server inside its memory limit while sending several hundred megabytes.
// Like the export, that costs the ability to report a failure partway through: the
// status is already sent, so a backup that fails halfway ends as a short download. The
// answer is the same as the export's — the file says whether it is whole, and
// `tome backup --verify` is how you ask it.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if s.blobRoot == "" {
		// Nothing to copy, and saying so beats a zero-file archive that looks like a
		// backup of an empty archive.
		s.log.Warn("a backup was requested with no archive tree configured")
		http.Error(w, "this instance has no archive tree configured, so there is nothing to back up",
			http.StatusServiceUnavailable)
		return
	}

	if !backupRunning.CompareAndSwap(false, true) {
		http.Error(w, "a backup is already running; wait for it to finish", http.StatusConflict)
		return
	}
	defer backupRunning.Store(false)

	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(backupDeadline)); err != nil {
		// Not fatal: the handler still works and inherits the server's own timeout,
		// which is enough for an archive small enough not to notice.
		s.log.Warn("could not extend the write deadline for a backup", "error", err)
	}

	filename := "tomekeeper-backup-" + time.Now().UTC().Format("2006-01-02") + ".tar"
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// Nothing about this is cacheable: it is a snapshot of an archive that changes, and
	// a stale one masquerading as a backup is worse than none.
	w.Header().Set("Cache-Control", "no-store")

	result, err := backup.Write(r.Context(), s.store.Pool(), w, backup.Options{
		BlobRoot: s.blobRoot,
		Version:  version.Short(),
	})
	if err != nil {
		// The status is long gone, so this cannot become a 500. Logged with the reader
		// who asked, because the download they are holding is short and only the log
		// can say why.
		s.log.Error("a backup failed partway through", "user_id", signedInUser(r), "error", err)
		return
	}

	s.log.Info("streamed a backup",
		"user_id", signedInUser(r),
		"bytes", result.Bytes,
		"files", len(result.Manifest.Files),
		"tables", len(result.Manifest.Tables),
		"missing", len(result.Manifest.Missing),
		"schema", result.Manifest.SchemaVersion)
}

// backupSummary is what the accounts page says about the download it offers.
type backupSummary struct {
	// Available says whether a backup can be taken at all, which needs an archive
	// tree this process can read.
	Available bool

	// Filename is what the browser will save, shown so that what arrives is not a
	// surprise.
	Filename string
}

func (s *Server) backupSummaryFor() backupSummary {
	return backupSummary{
		Available: s.blobRoot != "",
		Filename: fmt.Sprintf("tomekeeper-backup-%s.tar",
			time.Now().UTC().Format("2006-01-02")),
	}
}
