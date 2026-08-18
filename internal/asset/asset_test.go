package asset_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/asset"
)

// photograph makes a noisy image, which is what a real photo looks like to a
// compressor. A flat gradient compresses so well that it would make every
// size assertion meaningless.
func photograph(t *testing.T, width, height int) image.Image {
	t.Helper()

	// Deterministic seed: the test asserts on encoded sizes, and those must not
	// vary between runs.
	rng := rand.New(rand.NewPCG(1, 2))

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + int(rng.Uint32()%40)) % 256),
				G: uint8((y*5 + int(rng.Uint32()%40)) % 256),
				B: uint8((x + y + int(rng.Uint32()%40)) % 256),
				A: 255,
			})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding fixture: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding fixture: %v", err)
	}
	return buf.Bytes()
}

func TestProcessTranscodes(t *testing.T) {
	source := encodePNG(t, photograph(t, 800, 600))

	got, err := asset.Process(source, "image/png")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}

	if got.SHA256 == "" {
		t.Error("no content hash")
	}
	if len(got.Bytes) == 0 {
		t.Fatal("no bytes produced")
	}
	// The whole point of the pipeline: fewer bytes than arrived.
	if len(got.Bytes) >= len(source) {
		t.Errorf("stored %d bytes for a %d byte source — transcoding did not help",
			len(got.Bytes), len(source))
	}
	if got.MediaType != asset.FormatAVIF {
		t.Errorf("MediaType = %q, want AVIF as the first choice", got.MediaType)
	}
	if !strings.HasSuffix(got.Path, ".avif") {
		t.Errorf("Path = %q, want an .avif extension", got.Path)
	}
	if got.Width != 800 || got.Height != 600 {
		t.Errorf("dimensions = %dx%d, want 800x600 unchanged", got.Width, got.Height)
	}
}

// The asset policy: the long edge is capped at 1600, preserving aspect ratio.
func TestProcessDownscales(t *testing.T) {
	tests := []struct {
		name                  string
		width, height         int
		wantWidth, wantHeight int
	}{
		{"landscape over the cap", 3200, 1800, 1600, 900},
		{"portrait over the cap", 1800, 3200, 900, 1600},
		{"square over the cap", 2000, 2000, 1600, 1600},
		{"already small enough", 1200, 800, 1200, 800},
		{"exactly at the cap", 1600, 1000, 1600, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := encodeJPEG(t, photograph(t, tt.width, tt.height))

			got, err := asset.Process(source, "image/jpeg")
			if err != nil {
				t.Fatalf("Process() = %v", err)
			}
			if got.Width != tt.wantWidth || got.Height != tt.wantHeight {
				t.Errorf("dimensions = %dx%d, want %dx%d",
					got.Width, got.Height, tt.wantWidth, tt.wantHeight)
			}
		})
	}
}

// Content-addressing is on the **original** bytes, so that changing the
// encoder does not turn every image into a second copy of itself.
func TestContentAddressUsesOriginalBytes(t *testing.T) {
	source := encodePNG(t, photograph(t, 400, 300))

	first, err := asset.Process(source, "image/png")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}
	second, err := asset.Process(source, "image/png")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}

	if first.SHA256 != second.SHA256 {
		t.Errorf("the same source produced two addresses: %s and %s", first.SHA256, second.SHA256)
	}
	if first.Path != second.Path {
		t.Errorf("the same source produced two paths: %s and %s", first.Path, second.Path)
	}

	// A different image must land somewhere else.
	other, err := asset.Process(encodePNG(t, photograph(t, 401, 300)), "image/png")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}
	if other.SHA256 == first.SHA256 {
		t.Error("two different images share a content address")
	}
}

// Both conditions must hold to skip: small *and* tiny. A well-optimized
// diagram is small in bytes and large in pixels, and belongs in the archive.
func TestSizePolicy(t *testing.T) {
	tests := []struct {
		name     string
		width    int
		height   int
		wantSkip bool
	}{
		{"tracking pixel", 1, 1, true},
		{"spacer", 8, 8, true},
		{"small icon", 32, 32, true},
		{"just under both thresholds", 99, 99, true},
		{"wide and thin but large enough", 400, 20, false},
		{"ordinary illustration", 640, 480, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A flat color keeps these encodings tiny, so the byte threshold is
			// what the dimension rule has to be tested against.
			img := image.NewRGBA(image.Rect(0, 0, tt.width, tt.height))
			for y := range tt.height {
				for x := range tt.width {
					img.Set(x, y, color.RGBA{200, 200, 200, 255})
				}
			}
			source := encodePNG(t, img)

			_, err := asset.Process(source, "image/png")
			reason, skipped := asset.IsSkipped(err)

			if skipped != tt.wantSkip {
				t.Errorf("skipped = %v (%s), want %v for a %dx%d image of %d bytes",
					skipped, reason, tt.wantSkip, tt.width, tt.height, len(source))
			}
			if skipped && reason != asset.SkipTooSmall {
				t.Errorf("skip reason = %q, want %q", reason, asset.SkipTooSmall)
			}
		})
	}
}

func TestSVGHandling(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="200" height="100">` +
		`<rect width="200" height="100" fill="#333"/></svg>`)

	got, err := asset.Process(svg, "image/svg+xml")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}
	if got.MediaType != "image/svg+xml" {
		t.Errorf("MediaType = %q, want the SVG kept as SVG", got.MediaType)
	}
	if !bytes.Equal(got.Bytes, svg) {
		t.Error("the SVG was modified; it should be stored byte for byte")
	}
	if !strings.HasSuffix(got.Path, ".svg") {
		t.Errorf("Path = %q, want an .svg extension", got.Path)
	}

	// Sniffed even when the server mislabels it, which servers do constantly.
	if got, err = asset.Process(svg, "text/plain"); err != nil {
		t.Errorf("Process() = %v for a mislabeled SVG", err)
	} else if got.MediaType != "image/svg+xml" {
		t.Errorf("MediaType = %q, want the SVG detected by sniffing", got.MediaType)
	}

	// An SVG over the cap is a plotting tool that embedded its dataset.
	huge := append([]byte(`<svg xmlns="http://www.w3.org/2000/svg">`),
		bytes.Repeat([]byte("<path d='M0 0'/>"), 100_000)...)
	if _, err := asset.Process(huge, "image/svg+xml"); err == nil {
		t.Error("Process() accepted an oversize SVG")
	} else if reason, _ := asset.IsSkipped(err); reason != asset.SkipOversizeSVG {
		t.Errorf("skip reason = %q, want %q", reason, asset.SkipOversizeSVG)
	}
}

func TestUndecodableIsSkipped(t *testing.T) {
	_, err := asset.Process([]byte("this is not an image at all, it is prose"), "image/png")

	reason, skipped := asset.IsSkipped(err)
	if !skipped {
		t.Fatalf("Process() = %v, want a skip", err)
	}
	if reason != asset.SkipUndecodable {
		t.Errorf("skip reason = %q, want %q", reason, asset.SkipUndecodable)
	}
}

func TestShouldFetch(t *testing.T) {
	tests := []struct {
		url     string
		want    bool
		wantWhy asset.SkipReason
	}{
		{"https://example.com/photo.jpg", true, asset.SkipNone},
		{"http://example.com/photo.jpg", true, asset.SkipNone},
		{"data:image/png;base64,iVBORw0KG", false, asset.SkipDataURI},
		{"DATA:image/gif;base64,R0lGOD", false, asset.SkipDataURI},
		{"/relative/photo.jpg", false, asset.SkipNotHTTP},
		{"ftp://example.com/photo.jpg", false, asset.SkipNotHTTP},
		{"", false, asset.SkipNotHTTP},
	}

	for _, tt := range tests {
		got, why := asset.ShouldFetch(tt.url)
		if got != tt.want || why != tt.wantWhy {
			t.Errorf("ShouldFetch(%q) = %v, %q; want %v, %q", tt.url, got, why, tt.want, tt.wantWhy)
		}
	}
}

// The asset policy: pick the candidate nearest 1600 rather than the largest, so a 4000px
// original is not fetched only to be immediately downscaled.
func TestSelectFromSrcset(t *testing.T) {
	tests := []struct {
		name   string
		srcset string
		want   string
	}{
		{
			name:   "nearest to the target wins",
			srcset: "small.jpg 400w, medium.jpg 800w, large.jpg 1500w, huge.jpg 4000w",
			want:   "large.jpg",
		},
		{
			name:   "exact match",
			srcset: "a.jpg 800w, b.jpg 1600w, c.jpg 3200w",
			want:   "b.jpg",
		},
		{
			name:   "everything below the target picks the largest of them",
			srcset: "a.jpg 200w, b.jpg 400w, c.jpg 800w",
			want:   "c.jpg",
		},
		{
			name:   "everything above the target picks the smallest of them",
			srcset: "a.jpg 2000w, b.jpg 3000w, c.jpg 4000w",
			want:   "a.jpg",
		},
		{
			name:   "density descriptors have no width and are treated as the target",
			srcset: "a.jpg 1x, b.jpg 2x",
			want:   "a.jpg",
		},
		{
			name:   "a bare URL",
			srcset: "only.jpg",
			want:   "only.jpg",
		},
		{
			name:   "extra whitespace",
			srcset: "  a.jpg   400w ,   b.jpg   1600w  ",
			want:   "b.jpg",
		},
		{
			name:   "empty",
			srcset: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asset.SelectFromSrcset(tt.srcset); got != tt.want {
				t.Errorf("SelectFromSrcset(%q) = %q, want %q", tt.srcset, got, tt.want)
			}
		})
	}
}

// The two levels of prefix directory keep any one directory small enough for a
// file manager to open.
func TestBlobPath(t *testing.T) {
	const sha = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

	got := asset.BlobPath(sha, ".avif")
	if want := "assets/sha256/a1/b2/" + sha + ".avif"; got != want {
		t.Errorf("BlobPath() = %q, want %q", got, want)
	}

	// Defensive: a short hash must not panic on slicing.
	if got := asset.BlobPath("ab", ".avif"); got == "" {
		t.Error("BlobPath() with a short hash returned nothing")
	}
}

// A source that transcodes to something larger is kept as it arrived. This is
// not hypothetical: the WebP encoder here is lossless-only, so a photograph
// re-encoded losslessly is routinely bigger than its JPEG.
func TestTranscodeNeverGrowsAnImage(t *testing.T) {
	// A JPEG at quality 90 of noise is already near the entropy floor.
	source := encodeJPEG(t, photograph(t, 500, 400))

	got, err := asset.Process(source, "image/jpeg")
	if err != nil {
		t.Fatalf("Process() = %v", err)
	}

	if len(got.Bytes) > len(source) {
		t.Errorf("stored %d bytes for a %d byte source: the pipeline made the archive bigger",
			len(got.Bytes), len(source))
	}
	if !got.Transcoded && got.MediaType != "image/jpeg" {
		t.Errorf("MediaType = %q but Transcoded is false", got.MediaType)
	}
}

func TestOversizeSourceIsSkipped(t *testing.T) {
	_, err := asset.Process(make([]byte, 21<<20), "image/jpeg")

	reason, skipped := asset.IsSkipped(err)
	if !skipped || reason != asset.SkipOversizeFile {
		t.Errorf("Process() = %v (%q), want a %q skip", err, reason, asset.SkipOversizeFile)
	}
}

// bombPNG builds a PNG whose header claims an enormous image but which carries
// no pixel data at all.
//
// This is exactly the shape that matters: the file is a few hundred bytes, so
// every byte-size limit in the policy passes it, and decoding it would try to
// allocate width × height × 4. If the guard is working, the header alone is
// enough to reject it and the test costs nothing to run.
func bombPNG(t *testing.T, width, height uint32) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, width)
	_ = binary.Write(&ihdr, binary.BigEndian, height)
	ihdr.Write([]byte{8, 6, 0, 0, 0}) // 8-bit RGBA, no interlace

	_ = binary.Write(&buf, binary.BigEndian, uint32(ihdr.Len()-4))
	buf.Write(ihdr.Bytes())
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))

	return buf.Bytes()
}

func TestProcessRejectsDecompressionBombsFromTheHeader(t *testing.T) {
	// 20,000 x 20,000 = 400 megapixels, which would be 1.6GB decoded as RGBA.
	// The file itself is well under a kilobyte.
	raw := bombPNG(t, 20000, 20000)

	if len(raw) > asset.MinBytes {
		t.Fatalf("the fixture is %d bytes, which is not small enough to prove the point", len(raw))
	}

	_, err := asset.Process(raw, "image/png")

	var skipped *asset.ErrSkipped
	if !errors.As(err, &skipped) {
		t.Fatalf("Process() = %v, want a skip; a bomb this size must not reach image.Decode", err)
	}
	if skipped.Reason != asset.SkipTooManyPixels {
		t.Errorf("skip reason = %q, want %q", skipped.Reason, asset.SkipTooManyPixels)
	}
}

// Just under the limit must still be processed, so the guard cannot be satisfied
// by rejecting everything.
func TestProcessAcceptsLargeButReasonableImages(t *testing.T) {
	raw := bombPNG(t, 5000, 5000) // 25 megapixels, under the 30M limit

	_, err := asset.Process(raw, "image/png")

	var skipped *asset.ErrSkipped
	if errors.As(err, &skipped) && skipped.Reason == asset.SkipTooManyPixels {
		t.Errorf("a 25 megapixel image was rejected as too many pixels, so the limit is too tight")
	}
}
