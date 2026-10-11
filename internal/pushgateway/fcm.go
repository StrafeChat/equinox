package pushgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFCMBaseURL is where FCM HTTP v1 lives; tests point the client elsewhere.
	DefaultFCMBaseURL = "https://fcm.googleapis.com"
	fcmScope          = "https://www.googleapis.com/auth/firebase.messaging"
	// fcmMaxData is FCM's limit on a message's data payload (keys plus values).
	fcmMaxData = 4096
	// fcmTokenEarlyRefresh renews the OAuth2 access token this long before Google says it
	// expires, so a token never lapses between being read and being used.
	fcmTokenEarlyRefresh = 5 * time.Minute
)

// fcmClient sends data-only messages through FCM HTTP v1, authenticating as the Firebase
// service account with the OAuth2 JWT-bearer grant.
type fcmClient struct {
	creds   *FCMCredentials
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func newFCMClient(creds *FCMCredentials, baseURL string, client *http.Client, now func() time.Time) *fcmClient {
	return &fcmClient{creds: creds, baseURL: strings.TrimRight(baseURL, "/"), http: client, now: now}
}

type fcmRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token string `json:"token"`
	// Data only, never a `notification` block: the app decrypts and renders every push
	// itself, and a notification block would make Android show the fallback on its own.
	Data    map[string]string `json:"data"`
	Android fcmAndroid        `json:"android"`
}

type fcmAndroid struct {
	Priority    string `json:"priority"`
	TTL         string `json:"ttl"`
	CollapseKey string `json:"collapse_key,omitempty"`
}

func (c *fcmClient) Send(ctx context.Context, m *Message) error {
	body := b64.EncodeToString(m.Body)
	if len("s")+len(m.Reg.Slot)+len("p")+len(body) > fcmMaxData {
		return &DeliveryError{Kind: deliveryTooLarge, Reason: "exceeds FCM data limit"}
	}
	priority := "NORMAL"
	if m.alert() {
		priority = "HIGH"
	}
	payload, err := json.Marshal(fcmRequest{Message: fcmMessage{
		Token: m.Reg.Token,
		Data:  map[string]string{"s": m.Reg.Slot, "p": body},
		Android: fcmAndroid{
			Priority:    priority,
			TTL:         strconv.Itoa(m.TTL) + "s",
			CollapseKey: m.Topic,
		},
	}})
	if err != nil {
		return err
	}

	// A 401 means Google no longer accepts our cached access token (revoked, or the clock
	// moved); fetch a fresh one and try exactly once more.
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		status, header, respBody, err := c.post(ctx, token, payload)
		if err != nil {
			return networkError(err)
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.invalidate(token)
			continue
		}
		return classifyFCM(status, header, respBody, c.now())
	}
}

func (c *fcmClient) post(ctx context.Context, token string, payload []byte) (int, http.Header, []byte, error) {
	endpoint := c.baseURL + "/v1/projects/" + url.PathEscape(c.creds.ProjectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, readUpstream(resp.Body), nil
}

// fcmErrorBody is Google's error envelope for FCM v1.
type fcmErrorBody struct {
	Error struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			Type            string `json:"@type"`
			ErrorCode       string `json:"errorCode"`
			FieldViolations []struct {
				Field string `json:"field"`
			} `json:"fieldViolations"`
		} `json:"details"`
	} `json:"error"`
}

// classifyFCM turns FCM's answer into nil or a DeliveryError.
func classifyFCM(status int, header http.Header, body []byte, now time.Time) error {
	if status >= 200 && status < 300 {
		return nil
	}
	var eb fcmErrorBody
	_ = json.Unmarshal(body, &eb)
	code := eb.Error.Status
	tokenField := false
	for _, d := range eb.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
		for _, v := range d.FieldViolations {
			if v.Field == "message.token" {
				tokenField = true
			}
		}
	}
	switch {
	// Only the explicit error code counts: a bare 404 is also what a wrong project id
	// gets, and reading that as "every token is dead" would wipe every subscription.
	case code == "UNREGISTERED":
		return &DeliveryError{Kind: deliveryGone, Upstream: status, Reason: "UNREGISTERED"}
	case status == http.StatusBadRequest && code == "INVALID_ARGUMENT" &&
		(tokenField || strings.Contains(strings.ToLower(eb.Error.Message), "registration token")):
		return &DeliveryError{Kind: deliveryGone, Upstream: status, Reason: "INVALID_ARGUMENT token"}
	case code == "SENDER_ID_MISMATCH":
		// The token belongs to a different Firebase project than this gateway's credentials.
		// That is never one device's state: it is this gateway configured with another
		// project's service account (or an app build with another project's config), and it
		// hits every token alike. Answering 410 would have every instance delete every
		// subscription; 502 keeps them until the operator fixes the credentials.
		return &DeliveryError{Kind: deliveryUpstream, Upstream: status, Reason: code}
	case status == http.StatusTooManyRequests || code == "QUOTA_EXCEEDED":
		return &DeliveryError{Kind: deliveryRateLimited, Upstream: status, Reason: "QUOTA_EXCEEDED",
			RetryAfter: upstreamRetryAfter(header, now)}
	case status == http.StatusRequestEntityTooLarge:
		return &DeliveryError{Kind: deliveryTooLarge, Upstream: status, Reason: "payload too large"}
	default:
		if code == "" {
			code = http.StatusText(status)
		}
		return &DeliveryError{Kind: deliveryUpstream, Upstream: status, Reason: code}
	}
}

// accessToken returns a cached OAuth2 access token, exchanging a fresh service-account
// assertion when the cached one is about to expire. The lock is held across the exchange
// so a burst of pushes after expiry costs one round trip to Google, not one each.
func (c *fcmClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.expiry.Add(-fcmTokenEarlyRefresh)) {
		return c.token, nil
	}
	token, lifetime, err := c.exchange(ctx, now)
	if err != nil {
		return "", err
	}
	c.token, c.expiry = token, now.Add(lifetime)
	return token, nil
}

func (c *fcmClient) invalidate(token string) {
	c.mu.Lock()
	if c.token == token {
		c.token = ""
	}
	c.mu.Unlock()
}

// exchange performs the OAuth2 JWT-bearer grant (RFC 7523) at the service account's
// token_uri.
func (c *fcmClient) exchange(ctx context.Context, now time.Time) (string, time.Duration, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if c.creds.PrivateKeyID != "" {
		header["kid"] = c.creds.PrivateKeyID
	}
	assertion, err := signRS256(c.creds.PrivateKey, header, map[string]any{
		"iss":   c.creds.ClientEmail,
		"scope": fcmScope,
		"aud":   c.creds.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", 0, err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.creds.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, networkError(err)
	}
	defer resp.Body.Close()
	body := readUpstream(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Our credentials, not the device: a 502 the instance retries, while the operator
		// sees the cause in the log.
		var oe struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oe)
		reason := "oauth " + strconv.Itoa(resp.StatusCode)
		if oe.Error != "" {
			reason += " " + oe.Error
		}
		return "", 0, &DeliveryError{Kind: deliveryUpstream, Upstream: resp.StatusCode, Reason: reason}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", 0, &DeliveryError{Kind: deliveryUpstream, Upstream: resp.StatusCode, Reason: "oauth: no access_token"}
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	return tok.AccessToken, lifetime, nil
}
