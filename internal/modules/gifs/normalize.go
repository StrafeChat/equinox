package gifs

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Rendition preference, kept exactly as the web client had it (src/lib/gif/providers.ts)
// when it parsed the provider's answer itself. Preview: ~200px animated, a small file for the
// grid. Full: a downsized GIF for the message, falling back to the original. Still: one
// frame for the placeholder.
var (
	previewKeys = []string{"fixed_width", "fixed_width_downsampled", "fixed_height", "downsized", "preview_gif", "original"}
	fullKeys    = []string{"downsized", "downsized_medium", "downsized_large", "original", "fixed_width"}
	stillKeys   = []string{"fixed_width_still", "fixed_height_still", "480w_still", "original_still"}
)

// The provider's JSON is decoded piece by piece rather than into one strict struct, so one
// malformed item (or a size sent as a string) costs that item, not the whole page.
type rawResponse struct {
	Data       []json.RawMessage `json:"data"`
	Pagination json.RawMessage   `json:"pagination"`
}

type rawPagination struct {
	Offset     optNum `json:"offset"`
	Count      optNum `json:"count"`
	TotalCount optNum `json:"total_count"`
}

type rawItem struct {
	ID     json.RawMessage            `json:"id"`
	Title  json.RawMessage            `json:"title"`
	Images map[string]json.RawMessage `json:"images"`
}

type rendition struct {
	URL    string  `json:"url"`
	Width  flexInt `json:"width"`
	Height flexInt `json:"height"`
}

// flexInt is a size GIPHY sends as a string ("200") and some compatible APIs as a number.
// Anything else reads as 0 instead of failing the decode.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	*f = 0
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexInt(leadingInt(s))
		return nil
	}
	var n float64
	if json.Unmarshal(b, &n) == nil {
		*f = flexInt(clampSize(n))
	}
	return nil
}

// optNum is a pagination field that counts only when it really is a JSON number - the
// same test the web client made with typeof === 'number'.
type optNum struct {
	n  int
	ok bool
}

func (o *optNum) UnmarshalJSON(b []byte) error {
	var n float64
	if json.Unmarshal(b, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
		*o = optNum{n: int(math.Max(math.Min(n, math.MaxInt32), math.MinInt32)), ok: true}
	}
	return nil
}

// leadingInt reads a size the way JavaScript's parseInt does ("200", " 200", "200px"),
// giving 0 for anything without a leading number.
func leadingInt(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return int(clampSize(float64(n)))
}

// clampSize keeps a pixel size sane: never negative, never large enough to overflow.
func clampSize(n float64) float64 {
	if math.IsNaN(n) || n < 0 {
		return 0
	}
	return math.Min(math.Floor(n), 1<<20)
}

// normalize turns a provider's GIPHY-shaped answer into a Page. offset is the one that was
// asked for, used when the provider does not say where this page started.
func normalize(body []byte, offset int) (Page, error) {
	var raw rawResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Page{}, err
	}
	results := make([]Result, 0, len(raw.Data))
	for _, item := range raw.Data {
		if r, ok := toResult(item); ok {
			results = append(results, r)
		}
	}

	var pag rawPagination
	_ = json.Unmarshal(raw.Pagination, &pag) // missing or malformed: every field falls back
	next := offset
	if pag.Offset.ok {
		next = pag.Offset.n
	}
	if pag.Count.ok {
		next += pag.Count.n
	} else {
		next += len(results)
	}
	total := next + len(results)
	if pag.TotalCount.ok {
		total = pag.TotalCount.n
	}
	// Providers cannot page past offset 4999, and this server clamps a request to that. A
	// total beyond it would have the client keep asking for "more" and get the last page
	// again each time, so report only what can actually be paged to.
	total = min(total, maxOffset+1)
	return Page{Results: results, NextOffset: next, TotalCount: total}, nil
}

// toResult picks the renditions for one item, or reports false for an item the client could
// not show: no usable preview or full rendition, or one that is not https. A plain-http
// rendition would be mixed content in the client and a URL anyone on the path could swap.
func toResult(item json.RawMessage) (Result, bool) {
	var d rawItem
	if err := json.Unmarshal(item, &d); err != nil {
		return Result{}, false
	}
	preview, ok := pickRendition(d.Images, previewKeys)
	if !ok {
		return Result{}, false
	}
	full, ok := pickRendition(d.Images, fullKeys)
	if !ok {
		full = preview
	}
	if !isHTTPS(preview.URL) || !isHTTPS(full.URL) {
		return Result{}, false
	}
	r := Result{
		ID:            itemID(d.ID, full.URL),
		URL:           full.URL,
		Width:         int(full.Width),
		Height:        int(full.Height),
		PreviewURL:    preview.URL,
		PreviewWidth:  int(preview.Width),
		PreviewHeight: int(preview.Height),
	}
	var title string
	if json.Unmarshal(d.Title, &title) == nil {
		r.Title = title
	}
	// The still is optional: one that is missing or not https is left out, the GIF stays.
	if still, ok := pickRendition(d.Images, stillKeys); ok && isHTTPS(still.URL) {
		r.StillURL = still.URL
	}
	return r, true
}

// pickRendition returns the first rendition, in preference order, that carries a URL.
func pickRendition(images map[string]json.RawMessage, keys []string) (rendition, bool) {
	for _, k := range keys {
		raw, ok := images[k]
		if !ok {
			continue
		}
		var r rendition
		if json.Unmarshal(raw, &r) == nil && r.URL != "" {
			return r, true
		}
	}
	return rendition{}, false
}

// itemID is the provider's id as a string (GIPHY sends a string; a number is taken as
// written), else the GIF's own URL, which is unique enough to key a grid cell.
func itemID(raw json.RawMessage, fallback string) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] != '"' {
		var n json.Number
		if json.Unmarshal(t, &n) == nil && n != "" {
			return n.String()
		}
	}
	return fallback
}

func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
