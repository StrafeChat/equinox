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
