package asset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"path"

	// Decoders registered for image.Decode. GIF and PNG and JPEG are the
	// standard library's; WebP decoding comes from x/image.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/HugoSmits86/nativewebp"
	"github.com/gen2brain/avif"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Encoded formats, in the order the asset policy prefers them.
const (
	FormatAVIF     = "image/avif"
	FormatWebP     = "image/webp"
	FormatOriginal = "" // whatever the source was

	// avifQuality trades size against fidelity. 60 is the library default and
	// is visually indistinguishable for article illustrations at this size.
	avifQuality = 60

	// avifSpeed is the encoder's effort dial, 0 slowest to 10 fastest.
	//
	// 8, from measurement rather than from the library's "slower should make
	// for a better image in less bytes" guidance, which does not hold here.
	// Encoding one 1600x1200 photograph:
	//
	//	speed 4    92s    645 KB
	//	speed 6    16s    607 KB
	//	speed 8     6s    594 KB   <- smallest and fast
	//	speed 10    1s    741 KB
	//
	// Speed 8 dominates 6 and 4 on both axes, so there is nothing to trade.
	// Re-measure before changing it: this is the difference between an archive
	// that keeps up with a day's reading and one that does not.
	avifSpeed = 8
)

// ErrSkipped means the image was deliberately not localized. The reason says
// which rule applied.
type ErrSkipped struct {
	Reason SkipReason
}

func (e *ErrSkipped) Error() string { return "skipped: " + string(e.Reason) }

// Processed is one image ready to be stored.
type Processed struct {
	// SHA256 is the hash of the **original** bytes, before any resizing or
	// transcoding.
	//
	// Content-addressing on the source rather than the output is what makes
	// deduplication stable: change the encoder, change the quality setting,
	// change the target dimension, and the same source image still resolves to
	// the same address instead of silently becoming a second copy of itself.
	SHA256 string

	// Bytes is what to write — transcoded, or the original when transcoding
	// would not have helped.
	Bytes []byte

	MediaType string
	Width     int
	Height    int

	// Path is where this belongs in the blob store.
	Path string

	// Transcoded is false when the original bytes were kept.
	Transcoded bool
}

// Process turns fetched image bytes into something ready to store.
//
// It decodes, applies the size policy, downscales, and transcodes. SVG is
// handled separately: it is text, cannot be decoded as a raster image, and is
// stored as-is when small enough.
func Process(raw []byte, sourceMediaType string) (Processed, error) {
	if len(raw) > MaxSourceBytes {
		return Processed{}, &ErrSkipped{Reason: SkipOversizeFile}
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	if isSVG(raw, sourceMediaType) {
		if len(raw) > MaxSVGBytes {
			return Processed{}, &ErrSkipped{Reason: SkipOversizeSVG}
		}
		// Vector images are already small and resolution-independent; there is
		// nothing a raster transcode could improve.
		return Processed{
			SHA256:    sha,
			Bytes:     raw,
			MediaType: "image/svg+xml",
			Path:      BlobPath(sha, ".svg"),
		}, nil
	}

	// The header first, so an image too large to decode is rejected before it is
	// decoded. DecodeConfig reads only enough to learn the dimensions, so the
	// allocation this prevents is never made. Doing this check after image.Decode
	// would be reading the bomb to find out whether it was one.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Processed{}, &ErrSkipped{Reason: SkipUndecodable}
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return Processed{}, &ErrSkipped{Reason: SkipTooManyPixels}
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Processed{}, &ErrSkipped{Reason: SkipUndecodable}
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	if tooSmallToKeep(len(raw), width, height) {
		return Processed{}, &ErrSkipped{Reason: SkipTooSmall}
	}

	resized := downscale(img)
	rb := resized.Bounds()

	encoded, mediaType := encode(resized, raw, sourceMediaType)

	return Processed{
		SHA256:     sha,
		Bytes:      encoded,
		MediaType:  mediaType,
		Width:      rb.Dx(),
		Height:     rb.Dy(),
		Path:       BlobPath(sha, extensionFor(mediaType, sourceMediaType)),
		Transcoded: mediaType != sourceMediaType,
	}, nil
}

// encode transcodes to AVIF, falling back to WebP, falling back to the
// original bytes.
//
// The last fallback is not only for encoder failures. nativewebp is
// lossless-only, so a photograph re-encoded as lossless WebP is routinely
// *larger* than the JPEG it came from — and this pipeline exists to reduce
// storage, not to spend it. Any output that is not smaller than the source is
// discarded in favor of the source.
func encode(img image.Image, original []byte, sourceMediaType string) ([]byte, string) {
	var buf bytes.Buffer
	err := avif.Encode(&buf, img, avif.Options{
		Quality:      avifQuality,
		QualityAlpha: avifQuality,
		Speed:        avifSpeed,
	})
	if err == nil && buf.Len() > 0 && buf.Len() < len(original) {
		return buf.Bytes(), FormatAVIF
	}

	buf.Reset()
	if err := nativewebp.Encode(&buf, img, nil); err == nil && buf.Len() < len(original) {
		return buf.Bytes(), FormatWebP
	}

	return original, sourceMediaType
}

// downscale resizes so the long edge is at most MaxDimension, preserving
// aspect ratio. Images already small enough are returned untouched.
func downscale(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	longest := max(width, height)
	if longest <= MaxDimension {
		return img
	}

	scale := float64(MaxDimension) / float64(longest)
	newWidth := max(int(float64(width)*scale), 1)
	newHeight := max(int(float64(height)*scale), 1)

	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	// CatmullRom over the faster kernels: this runs once per image, forever,
	// and the result is what a reader looks at for the next decade.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	return dst
}

// BlobPath returns where an asset lives in the blob store.
//
// The blob layout: assets/sha256/a1/b2/a1b2c3….avif. The two levels of prefix directories
// keep any single directory to a few thousand entries, which matters because
// some filesystems degrade badly with hundreds of thousands of files in one
// place — and because a human browsing the archive should be able to open a
// directory without their file manager freezing.
func BlobPath(sha, extension string) string {
	if len(sha) < 4 {
		return path.Join("assets", "sha256", sha+extension)
	}
	return path.Join("assets", "sha256", sha[0:2], sha[2:4], sha+extension)
}

func extensionFor(mediaType, sourceMediaType string) string {
	switch mediaType {
	case FormatAVIF:
		return ".avif"
	case FormatWebP:
		return ".webp"
	}

	switch sourceMediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".bin"
	}
}

// isSVG identifies vector images, which cannot be decoded as rasters.
func isSVG(raw []byte, mediaType string) bool {
	if mediaType == "image/svg+xml" {
		return true
	}

	// Sniff, because plenty of servers send SVG as text/plain or
	// application/octet-stream. Only the start of the file is examined; an
	// SVG document begins with an XML declaration, a comment, or the tag.
	head := raw
	if len(head) > 1024 {
		head = head[:1024]
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<svg"))
}

// IsSkipped reports whether an error is a deliberate skip rather than a
// failure, and returns the reason.
func IsSkipped(err error) (SkipReason, bool) {
	var skipped *ErrSkipped
	if errors.As(err, &skipped) {
		return skipped.Reason, true
	}
	return SkipNone, false
}
