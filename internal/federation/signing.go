package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Request signature scheme ("strafe-fed-v1").
//
// Headers on every server-to-server request:
//
//	X-Strafe-Instance:  sender's domain
//	X-Strafe-Timestamp: unix seconds (receiver rejects if more than MaxClockSkew away)
//	X-Strafe-Nonce:     random string, unique per request (receiver rejects replays)
//	X-Strafe-Signature: <key_id>=<base64 ed25519 signature>
//
// The signature covers the method, the federation-relative path (what follows
// /federation/v1, query string included - so reverse proxies that mount the API under a
// prefix don't change what's signed), the timestamp, the nonce and the SHA-256 of the body.
const (
	HeaderInstance  = "X-Strafe-Instance"
	HeaderTimestamp = "X-Strafe-Timestamp"
	HeaderNonce     = "X-Strafe-Nonce"
	HeaderSignature = "X-Strafe-Signature"

	signingPrefix = "strafe-fed-v1"
	// MaxClockSkewSeconds bounds how old/new a request timestamp may be.
	MaxClockSkewSeconds = 300
	// BasePath is where inbound federation routes are mounted on the API.
	BasePath = "/federation/v1"
)

func signingPayload(method, fedPath, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		signingPrefix,
		strings.ToUpper(method),
		fedPath,
		timestamp,
		nonce,
		hex.EncodeToString(sum[:]),
	}, "\n"))
}
