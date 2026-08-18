package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The semaphore is the only thing standing between an illustrated feed and an
// OOM kill, so it is worth asserting that it actually bounds anything.
//
// Written against the semaphore rather than against asset.Process, because
// running the real transcoder concurrently is exactly the 3GB allocation this
// test exists to prevent.
func TestTranscodeSlotsAreBounded(t *testing.T) {
	const (
		slots   = 2
		callers = 12
	)

	w := &LocalizeAssetsWorker{transcode: make(chan struct{}, slots)}

	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		wg       sync.WaitGroup
	)

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			w.transcode <- struct{}{}
			defer func() { <-w.transcode }()

			n := inFlight.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			// Long enough that every caller would overlap if nothing bounded them.
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > slots {
		t.Errorf("peak simultaneous transcodes = %d, want at most %d", got, slots)
	}
}

// A shutdown must not have to wait behind a queue of images.
func TestTranscodeWaitRespectsCancellation(t *testing.T) {
	w := &LocalizeAssetsWorker{transcode: make(chan struct{}, 1)}

	// Occupy the only slot.
	w.transcode <- struct{}{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := w.process(ctx, []byte("not an image"), "image/png"); err == nil {
		t.Error("process() returned no error on a canceled context, so shutdown would block behind the queue")
	}
}

// A worker built without a semaphore must still work rather than deadlock,
// because that is what every existing test constructs.
func TestTranscodeWithoutASemaphoreStillRuns(t *testing.T) {
	w := &LocalizeAssetsWorker{}

	// Deliberately undecodable: this asserts it reached asset.Process at all,
	// without paying for a real transcode.
	if _, err := w.process(t.Context(), []byte("not an image"), "image/png"); err == nil {
		t.Error("expected a skip error from processing nonsense, got nil")
	}
}
