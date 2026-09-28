package nebula

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Put uploads an object to Nebula at key (e.g. avatars/123/abc.png).
func Put(ctx context.Context, baseURL, uploadSecret, key string, body io.Reader, contentType string, size int64) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("nebula: empty base URL")
	}
	u := baseURL + "/v1/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+uploadSecret)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if size > 0 {
		req.ContentLength = size
	}
	client := &http.Client{Timeout: 120 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("nebula: PUT %s: %s: %s", key, res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Delete removes an object from Nebula. A missing object is not an error - callers use
// this for best-effort cleanup (deleted messages, replaced emoji) where "already gone" is
// the desired end state.
func Delete(ctx context.Context, baseURL, uploadSecret, key string) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("nebula: empty base URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/v1/"+key, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+uploadSecret)
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("nebula: DELETE %s: %s: %s", key, res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// PublicBase is the browser-facing base URL for objects: the configured public URL, or
// the internal base when none is set.
func PublicBase(baseURL, publicURL string) string {
	if p := strings.TrimRight(strings.TrimSpace(publicURL), "/"); p != "" {
		return p
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// ObjectURL is the public URL for an object key.
func ObjectURL(baseURL, publicURL, key string) string {
	return PublicBase(baseURL, publicURL) + "/v1/" + key
}

// KeyFromURL recovers the object key from a URL produced by ObjectURL ("" if it isn't one).
func KeyFromURL(url string) string {
	i := strings.Index(url, "/v1/")
	if i < 0 {
		return ""
	}
	return strings.Trim(url[i+len("/v1/"):], "/")
}
