// Package gifs searches GIF providers (GIPHY, and Heypster's GIPHY-compatible API) on the
// clients' behalf. It exists so the providers' API keys never leave this server: the web
// client used to call the provider straight from the browser, which meant publishing the key
// in config.js for anyone to lift, and sending every user's address and search text to the
// provider. Now the provider only ever sees this server, and the client gets a normalized
// page with nothing in it but GIF URLs and their sizes.
package gifs

import "errors"

// Result is one GIF, normalized from the provider's rendition set. The web client's
// GifResult is this, in camelCase.
type Result struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// URL is the rendition that gets sent as the message and rendered inline.
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	// Preview is the smaller animated rendition shown in the picker grid.
	PreviewURL    string `json:"preview_url"`
	PreviewWidth  int    `json:"preview_width"`
	PreviewHeight int    `json:"preview_height"`
	// StillURL is a single frame the picker shows while the animation loads.
	StillURL string `json:"still_url,omitempty"`
}

// Page is one page of results plus what the client needs to ask for the next one.
type Page struct {
	Results    []Result `json:"results"`
	NextOffset int      `json:"next_offset"`
	TotalCount int      `json:"total_count"`
}

var (
	ErrUnknownProvider = errors.New("unknown gif provider")
	ErrNotConfigured   = errors.New("gif provider not configured")
	ErrQueryRequired   = errors.New("query is required")
	// ErrUpstream covers everything that can go wrong past this server: the provider
	// unreachable, a non-2xx answer, a body too large or not the shape we expect. The
	// detail goes to the log (without the key); the client only learns it did not work.
	ErrUpstream = errors.New("gif provider unavailable")
	// ErrUpstreamThrottled is the provider answering 429: this instance's key is out of
	// quota for now, which is temporary in a way the other upstream failures may not be.
	ErrUpstreamThrottled = errors.New("gif provider rate limited")
)
