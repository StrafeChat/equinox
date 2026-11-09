package unfurl

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/StrafeChat/equinox/internal/imageprobe"
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
	return imageprobe.Size(head)
}
