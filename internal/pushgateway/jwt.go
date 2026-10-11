package pushgateway

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

// The gateway signs two kinds of JWT itself (APNs provider tokens, ES256; Google OAuth2
// assertions, RS256) and verifies one (VAPID, ES256). They are small enough that a JWT
// library would be more surface than help.

var b64 = base64.RawURLEncoding

// signingInput returns base64url(header) "." base64url(claims).
func signingInput(header, claims any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(h) + "." + b64.EncodeToString(c), nil
}

// signES256 produces a compact JWT whose signature is the JWS form of ES256: the raw,
// fixed-width r||s (64 bytes), not the ASN.1 DER that crypto/ecdsa emits by default.
func signES256(key *ecdsa.PrivateKey, header, claims any) (string, error) {
	input, err := signingInput(header, claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig), nil
}

// signRS256 produces a compact RS256 (RSASSA-PKCS1-v1_5 with SHA-256) JWT.
func signRS256(key *rsa.PrivateKey, header, claims any) (string, error) {
	input, err := signingInput(header, claims)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64.EncodeToString(sig), nil
}
