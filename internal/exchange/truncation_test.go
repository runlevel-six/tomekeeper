package exchange_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/exchange"
)

// Truncation, across both adapters and both shapes of it.
//
// A truncated export is the failure an importer has to get right, because it is the
// one that can look like success: every record before the cut is complete and
// readable, so an adapter that stopped at the last one would import 200 articles of
// 9,000 and print a cheerful summary. That is why the end of the array is checked
// explicitly and why running out of input is fatal.
//
// What these tests exist for beyond that, and why they are in their own file: the
// standard library reports running out of input as *two unrelated error values*
// depending on where the cut falls, and which one it uses for a given cut has moved
// between Go releases. That is not a detail — the version that treated one of them as
// a recoverable per-record problem did not terminate, because a decoder at the end of
// its input cannot advance, so `tome import` printed `record N: unexpected EOF` with
// N climbing until somebody killed it. Both shapes are pinned here, for both adapters,
// so the next release to move the line fails a test instead of shipping that.

// truncationCases are the two shapes, described by where the cut falls.
//
// Written as literals rather than fixtures so that the cut is visible in the test.
// Which standard-library error each one produces is deliberately not asserted: that is
// the thing that moves, and the behavior is what matters.
func truncationCases(t *testing.T) map[string]struct{ wallabag, tome string } {
	t.Helper()

	return map[string]struct{ wallabag, tome string }{
		// Complete records, then nothing. No closing bracket ever arrives, and this is
		// the dangerous one: there is no malformed record to point at.
		"between records": {
			wallabag: `[{"id":1,"url":"https://example.com/a","title":"A","is_archived":0,` +
				`"content":"<p>x</p>"}`,
			tome: `[{"schema_version":1,"source_name":"tomekeeper","source_id":"a",` +
				`"url":"https://example.com/a","title":"A"}`,
		},
		// Cut after a comma, so the decoder is expecting another record that never
		// starts.
		"after a comma": {
			wallabag: `[{"id":1,"url":"https://example.com/a","title":"A","is_archived":0,` +
				`"content":"<p>x</p>"},`,
			tome: `[{"schema_version":1,"source_name":"tomekeeper","source_id":"a",` +
				`"url":"https://example.com/a","title":"A"},`,
		},
		// Cut in the middle of a key, which is the shape that used to loop forever.
		"mid token": {
			wallabag: `[{"id":1,"url":"https://example.com/a","title":"A","is_archived":0,` +
				`"content":"<p>x</p>"},{"id":2,"url":`,
			tome: `[{"schema_version":1,"source_name":"tomekeeper","source_id":"a",` +
				`"url":"https://example.com/a","title":"A"},{"schema_version":1,"url":`,
		},
	}
}

// maxYields bounds every case below.
//
// Not tidiness: without a bound, a regression here does not fail the test, it hangs
// the package for the full ten-minute timeout and reports nothing useful. Three
// records in, anything past a handful of yields is a loop.
const maxYields = 50

// drain consumes an import, refusing to run away.
func drain(t *testing.T, imp exchange.Importer, export string) (articles int, errs []error) {
	t.Helper()

	for a, err := range imp.Import(t.Context(),
		exchange.Source{Path: "inline", Reader: strings.NewReader(export)}) {
		switch {
		case err != nil:
			errs = append(errs, err)
		case a != nil:
			articles++
		}

		if articles+len(errs) > maxYields {
			t.Fatalf("%s yielded more than %d times on a truncated file and was still going: "+
				"it does not terminate", imp.Name(), maxYields)
		}
	}
	return articles, errs
}

// A truncated export is fatal, says the file is incomplete, and stops.
//
// Three assertions, and each one is a separate thing that has been wrong:
//
//   - It terminates. The loop-forever bug.
//   - It is fatal, which is what keeps a truncated file out of the write pass — the
//     property `tome import`'s two passes rest on.
//   - It says the *file* is incomplete rather than naming a record. A cut between
//     records has no bad record to name, and the number it would land on is one past
//     the end: "record 3 of a 2-record file" sends an operator looking for something
//     that is not there.
func TestATruncatedExportIsFatalAndSaysSo(t *testing.T) {
	for name, cut := range truncationCases(t) {
		for _, adapter := range []struct {
			importer exchange.Importer
			export   string
		}{
			{exchange.Wallabag{}, cut.wallabag},
			{exchange.Tomekeeper{}, cut.tome},
		} {
			t.Run(adapter.importer.Name()+"/"+name, func(t *testing.T) {
				_, errs := drain(t, adapter.importer, adapter.export)

				if len(errs) == 0 {
					t.Fatal("a truncated export produced no error at all")
				}

				last := errs[len(errs)-1]

				var fatal *exchange.FatalError
				if !errors.As(last, &fatal) {
					t.Errorf("a truncated export was not fatal, so it would have been "+
						"written: %v", last)
				}
				if !strings.Contains(last.Error(), "ends before its last record") {
					t.Errorf("the error does not say the file is incomplete: %v", last)
				}

				// And it is the *only* error. A truncation is one fact about the file,
				// not a complaint per record — which is what the runaway produced.
				if len(errs) != 1 {
					t.Errorf("got %d errors, want the single truncation: %v", len(errs), errs)
				}
			})
		}
	}
}

// The records before the cut are still read, which is the whole reason a truncation
// needs checking rather than reporting itself.
func TestATruncatedExportStillYieldsWhatWasComplete(t *testing.T) {
	cases := truncationCases(t)

	for name, want := range map[string]int{
		"between records": 1,
		"after a comma":   1,
		"mid token":       1,
	} {
		t.Run(name, func(t *testing.T) {
			articles, _ := drain(t, exchange.Wallabag{}, cases[name].wallabag)
			if articles != want {
				t.Errorf("read %d complete records, want %d", articles, want)
			}
		})
	}
}

// A malformed record in an otherwise complete file is still a per-record problem.
//
// The counterweight to everything above: making truncation fatal must not make every
// decode failure fatal, or one bad entry in a library of thousands would cost the
// import. This is the case that keeps `isIncomplete` from being "any decode error".
func TestABadRecordInACompleteFileIsNotFatal(t *testing.T) {
	// A type error rather than a syntax error: `tags` is an object where the adapter
	// wants strings or labeled objects. The record is well-formed JSON, so the
	// decoder stays on a record boundary and the next record is readable.
	const export = `[
		{"id":1,"url":"https://example.com/a","title":"A","is_archived":0,"content":"<p>x</p>"},
		{"id":2,"url":"https://example.com/b","title":"B","is_archived":0,"tags":{"no":"pe"}},
		{"id":3,"url":"https://example.com/c","title":"C","is_archived":0,"content":"<p>z</p>"}
	]`

	articles, errs := drain(t, exchange.Wallabag{}, export)

	if articles != 2 {
		t.Errorf("read %d articles, want the 2 either side of the bad record", articles)
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1 for the bad record: %v", len(errs), errs)
	}

	var fatal *exchange.FatalError
	if errors.As(errs[0], &fatal) {
		t.Errorf("one unreadable record was treated as fatal, which would cost the "+
			"whole import: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "record 2") {
		t.Errorf("the error does not name the record it belongs to: %v", errs[0])
	}
}
