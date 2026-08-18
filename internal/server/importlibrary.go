package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

// maxLibraryUpload bounds an uploaded reading library.
//
// A read-later export is mostly article bodies, so it is orders of magnitude larger
// than a subscription list: the maintainer's own 385-entry Wallabag export is 6.5MB,
// which puts a decade of heavy use somewhere in the tens of megabytes. 128MB is past
// that with room to spare and still small enough that a video dropped on the wrong
// form is refused rather than buffered.
const maxLibraryUpload = 128 << 20

// maxLibraryMemory is how much of an upload stays in memory before spilling to a
// temporary file.
//
// Small on purpose, and it is what makes reading the file twice affordable: the
// multipart file is seekable, so the report pass and the write pass read the same
// spooled file rather than the same buffer in memory. Memory cost is therefore
// bounded by this rather than by how large an upload is allowed to be.
const maxLibraryMemory = 4 << 20

// handleImportLibrary imports a reading library somebody uploaded.
//
// The same two passes as the command line, for the same reason: the file is read
// once to report what importing it would do and again to do it, so a truncated or
// corrupt export fails before anything is written. Both passes read the uploaded
// file, which multipart spools to disk and which is seekable — no second upload and
// no holding a decade of articles in memory.
//
// Report-only is offered as a checkbox rather than a separate page, because it is
// the same request with the second pass skipped, and pretending otherwise would
// mean uploading a large file twice to find out what it holds.
func (s *Server) handleImportLibrary(w http.ResponseWriter, r *http.Request) {
	userID := signedInUser(r)

	// Before ParseMultipartForm, so an oversized body is refused as it arrives
	// instead of being spooled to disk first.
	r.Body = http.MaxBytesReader(w, r.Body, maxLibraryUpload)

	if err := r.ParseMultipartForm(maxLibraryMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.renderSaved(w, r, http.StatusRequestEntityTooLarge, nil, &libraryOutcome{
				Problem: "That file is larger than 128MB. If a library really is that big, " +
					"`tome import` on the command line has no such limit.",
			})
			return
		}
		s.log.Warn("reading a library upload failed", "error", err)
		s.renderSaved(w, r, http.StatusBadRequest, nil, &libraryOutcome{
			Problem: "The upload did not arrive intact. Try again.",
		})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("library")
	if err != nil {
		s.renderSaved(w, r, http.StatusBadRequest, nil, &libraryOutcome{
			Problem: "No file was chosen.",
		})
		return
	}
	defer func() { _ = file.Close() }()

	outcome := &libraryOutcome{
		Filename:   header.Filename,
		ReportOnly: r.PostFormValue("report_only") != "",
	}

	// Which format, from the first few kilobytes rather than the filename: a browser
	// names a download whatever it likes.
	head := make([]byte, exchange.DetectHead)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		s.log.Warn("reading the start of a library upload failed", "error", err)
		outcome.Problem = "The upload could not be read. Try again."
		s.renderSaved(w, r, http.StatusBadRequest, nil, outcome)
		return
	}

	imp := exchange.DetectImporterFor(head[:n])
	if imp == nil {
		outcome.Problem = "That file is not a format this build can read. It reads " +
			strings.Join(exchange.ImporterNames(), ", ") +
			" exports — in Wallabag, that is Settings → Export → JSON."
		s.renderSaved(w, r, http.StatusBadRequest, nil, outcome)
		return
	}
	outcome.Source = imp.Name()

	// Pass one: what would this do. Nothing is written.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		s.log.Warn("rewinding a library upload failed", "error", err)
		outcome.Problem = "The upload could not be re-read. Try again."
		s.renderSaved(w, r, http.StatusInternalServerError, nil, outcome)
		return
	}

	report, err := exchange.Inspect(r.Context(), s.store, userID, imp,
		exchange.Source{Path: header.Filename, Reader: file})
	if err != nil {
		s.log.Info("an uploaded library could not be read", "filename", header.Filename, "error", err)
		outcome.Problem = "That export could not be read, and nothing was imported: " + err.Error()
		s.renderSaved(w, r, http.StatusBadRequest, nil, outcome)
		return
	}
	outcome.Report = &report

	if outcome.ReportOnly {
		s.log.Info("reported on a library upload without importing",
			"filename", header.Filename, "records", report.Records)
		s.renderSaved(w, r, http.StatusOK, nil, outcome)
		return
	}

	// Pass two: do it.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		s.log.Warn("rewinding a library upload failed", "error", err)
		outcome.Problem = "The upload could not be re-read, so nothing was imported. Try again."
		s.renderSaved(w, r, http.StatusInternalServerError, nil, outcome)
		return
	}

	applied, err := exchange.Apply(r.Context(), s.store, userID, imp,
		exchange.Source{Path: header.Filename, Reader: file})
	if err != nil {
		s.log.Error("importing an uploaded library failed", "filename", header.Filename, "error", err)
		outcome.Problem = "The import stopped partway: " + err.Error() +
			" Re-uploading the file is safe and will continue where it left off."
		outcome.Report = &applied
		s.renderSaved(w, r, http.StatusInternalServerError, nil, outcome)
		return
	}
	outcome.Report = &applied

	for _, f := range applied.Written.Failures {
		s.log.Warn("a record in an uploaded library could not be imported",
			"record", f.Record, "error", f.Err)
	}

	s.log.Info("imported a library from an upload",
		"filename", header.Filename, "source", imp.Name(),
		"records", applied.Records, "articles", applied.Written.Articles,
		"failed", len(applied.Written.Failures))

	s.renderSaved(w, r, http.StatusOK, nil, outcome)
}

// libraryOutcome is what the reading list says about an upload that just happened.
type libraryOutcome struct {
	Filename string
	Source   string

	// ReportOnly records which question was asked, so the page can answer the one
	// that was: "here is what it holds" rather than "here is what was imported".
	ReportOnly bool

	// Problem is a reason nothing was imported.
	Problem string

	// Report is what the file holds and, unless ReportOnly, what importing it did.
	Report *exchange.Report
}
