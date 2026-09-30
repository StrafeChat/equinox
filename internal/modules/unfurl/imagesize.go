package unfurl

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"

	// Decoders for image.DecodeConfig: reading a header is enough to learn the size.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// probeBytes is how much of an image we fetch to read its header: every format we handle
// declares its size well within the first few KB, but a JPEG can carry a large EXIF/ICC
// block before its frame header, so allow for that.
const probeBytes = 96 * 1024

// fillImageSize learns the intrinsic size of a preview image the page didn't declare, by
// fetching just the start of the file (through the same SSRF-safe client) and decoding its
// header. Discord does the same on its side - it is what lets a client reserve the exact box
// before the image loads, so the conversation never shifts when it arrives. Best-effort: on
// any failure the card simply ships without a size, as before.
func (s *Service) fillImageSize(ctx context.Context, m *Metadata) {
	if m == nil || m.Image == nil || m.Image.URL == "" || (m.Image.Width > 0 && m.Image.Height > 0) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(m.Image.URL), "http") {
		return
	}
	if w, h, ok := s.probeImageSize(ctx, m.Image.URL); ok {
		m.Image.Width, m.Image.Height = w, h
	}
}

func (s *Service) probeImageSize(ctx context.Context, rawURL string) (int, int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, 0, false
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/*")
	req.Header.Set("Range", "bytes=0-"+strconv.Itoa(probeBytes-1))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, probeBytes))
	if err != nil && len(head) == 0 {
		return 0, 0, false
	}
	if w, h, ok := webpSize(head); ok {
		return w, h, true
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(head))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// webpSize reads the dimensions from a WebP header (the standard library has no WebP
// decoder). Handles the three container flavours: VP8 (lossy), VP8L (lossless) and VP8X
// (extended, with the canvas size in the chunk).
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		// Lossy: the frame header starts at byte 20; the 3-byte start code 9d 01 2a sits at
		// offsets 23-25, then 14-bit width and height (little-endian, top 2 bits are scale).
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
		return w, h, w > 0 && h > 0
	case "VP8L":
		// Lossless: signature byte 0x2f at 20, then 14 bits width-1 and 14 bits height-1.
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		w := int(bits&0x3fff) + 1
		h := int((bits>>14)&0x3fff) + 1
		return w, h, true
	case "VP8X":
		// Extended: canvas width-1 and height-1 as 24-bit little-endian at 24 and 27.
		w := int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
		h := int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
		return w, h, true
	}
	return 0, 0, false
}
