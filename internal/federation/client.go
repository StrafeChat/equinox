package federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
)

// Client makes signed requests to peer instances.
type Client struct {
	cfg       config.FederationConfig
	signer    *Signer
	discovery *Discovery
	http      *http.Client
}

func NewClient(cfg config.FederationConfig, signer *Signer, discovery *Discovery) *Client {
	return &Client{cfg: cfg, signer: signer, discovery: discovery, http: &http.Client{
		Timeout: 20 * time.Second,
		// Signed requests go exactly where the peer's instance document says; a redirect
		// would carry the signature (and body) to a URL nobody vetted.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// StatusError is a non-2xx reply from a peer.
type StatusError struct {
	Domain string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("federation: %s replied %d: %s", e.Domain, e.Status, e.Body)
}

func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Do sends a signed JSON request to domain's federation endpoint. fedPath is relative to
// /federation/v1 (e.g. "/rooms" or "/users/lookup?username=a"). A JSON
// 2xx body is decoded into out when out is non-nil.
func (c *Client) Do(ctx context.Context, domain, method, fedPath string, body any, out any) (int, error) {
	if !c.cfg.IsAllowedPeer(domain) {
		return 0, ErrPeerNotAllowed
	}
	info, err := c.discovery.Lookup(ctx, domain, false)
	if err != nil {
		return 0, err
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	if !strings.HasPrefix(fedPath, "/") {
		fedPath = "/" + fedPath
	}
	status, resBody, err := c.send(ctx, info, method, fedPath, payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	if status < 200 || status > 299 {
		return status, &StatusError{Domain: domain, Status: status, Body: strings.TrimSpace(string(resBody))}
	}
	if out != nil && len(resBody) > 0 {
		if err := json.Unmarshal(resBody, out); err != nil {
			return status, fmt.Errorf("federation: bad response from %s: %w", domain, err)
		}
	}
	return status, nil
}

func (c *Client) send(ctx context.Context, info *InstanceInfo, method, fedPath string, payload []byte) (int, []byte, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := newNonce()
	sig := c.signer.Sign(signingPayload(method, fedPath, ts, nonce, payload))

	req, err := http.NewRequestWithContext(ctx, method, info.FederationURL+fedPath, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "strafe-equinox-federation/1")
	req.Header.Set(HeaderInstance, c.cfg.Domain)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, c.signer.KeyID+"="+sig)
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4*1024*1024))
	return res.StatusCode, b, nil
}
