package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"os"
	"os/exec"

	_ "golang.org/x/image/webp" // WhatsApp stickers are WebP
)

// Target size for the inline preview. Rendered into a handful of terminal
// cells, so there's no point shipping a multi-megapixel photo: re-encoding
// full-resolution JPEGs as lossless PNG made both the encode and the
// base64 transfer through the pty painfully slow.
const (
	previewMaxW = 720
	previewMaxH = 450
)

type mediaDownloadResult struct {
	Path string `json:"path"`
}

func downloadMedia(chatJID, msgID string) (string, error) {
	var env envelope[mediaDownloadResult]
	if err := runWacli(&env, "media", "download", "--chat", chatJID, "--id", msgID); err != nil {
		return "", err
	}
	if !env.Success {
		return "", fmt.Errorf("wacli: %s", deref(env.Error))
	}
	if env.Data.Path == "" {
		return "", fmt.Errorf("media download: empty path in response")
	}
	return env.Data.Path, nil
}

// canExtractGifFrame reports whether ffmpeg is on PATH, needed to pull a
// still frame out of a "gif" message's underlying video file for the
// inline preview.
func canExtractGifFrame() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// extractVideoFrame pulls the first frame of a video file (WhatsApp "gif"
// messages are really short looping videos) out to a temp PNG file via
// ffmpeg, for loadImagePNG to downscale and re-encode like any other
// image. The caller must remove the returned path.
func extractVideoFrame(path string) (string, error) {
	tmp, err := os.CreateTemp("", "wacli-frame-*.png")
	if err != nil {
		return "", err
	}
	tmp.Close()
	cmd := exec.Command("ffmpeg", "-y", "-i", path, "-frames:v", "1", tmp.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return tmp.Name(), nil
}

// loadImagePNG reads an image file (jpeg, png, or webp — stickers are
// WebP — whatever Go's image package can decode by magic bytes) and
// re-encodes it as PNG, since the kitty graphics protocol's f=100 format
// only accepts PNG. It also returns the (pre-downscale) pixel dimensions,
// so the caller can size the terminal-cell box to match the image's
// actual aspect ratio. Animated WebP stickers decode as their first frame
// only; x/image/webp doesn't support animation.
func loadImagePNG(path string) (png_ []byte, w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()

	img = downscale(img, previewMaxW, previewMaxH)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

// downscale does a cheap nearest-neighbor resize down to fit within
// maxW x maxH, preserving aspect ratio. Quality doesn't matter much at
// the target size (a handful of terminal cells); speed does.
func downscale(img image.Image, maxW, maxH int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW && h <= maxH {
		return img
	}
	scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	dw, dh := int(float64(w)*scale), int(float64(h)*scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*h/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*w/dw
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	return dst
}
