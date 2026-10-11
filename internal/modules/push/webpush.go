package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// RFC 8291 message encryption ("aes128gcm" content coding, RFC 8188), sender side. Each
// message gets a fresh ephemeral P-256 key and a fresh salt, so two pushes to the same
// device share nothing an observer could link, and a key compromise on the server reveals
// no past push.

const (
	// recordSize is the rs header field. A push is always a single record well below it.
	recordSize = 4096
	// MaxPayloadBytes is the largest plaintext payload: the biggest padding bucket less the
	// one-byte record delimiter.
	MaxPayloadBytes = 2047
	saltLen         = 16
	authSecretLen   = 16
	keyLen          = 65 // uncompressed P-256 point
	headerLen       = saltLen + 4 + 1 + keyLen
	tagLen          = 16
)

// paddingBuckets are the record-plaintext lengths (payload + delimiter + padding) a push
// may have. Padding every body up to one of them hides message length from everyone in
// the middle; four sizes leak at most two bits.
var paddingBuckets = []int{256, 512, 1024, 2048}

var (
	errPayloadTooLarge = errors.New("push: payload larger than the largest padding bucket")
	errBadKeys         = errors.New("push: invalid subscription keys")
)

// decodeB64 accepts base64url with or without padding (the Web Push specs say unpadded;
// some clients pad anyway) and, leniently, the standard alphabet.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "+/") {
		return base64.RawStdEncoding.DecodeString(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// parseReceiverKeys decodes a subscription's p256dh (an uncompressed point that must lie
// on P-256 - crypto/ecdh checks) and auth secret (16 bytes).
func parseReceiverKeys(p256dh, auth string) (*ecdh.PublicKey, []byte, error) {
	raw, err := decodeB64(p256dh)
	if err != nil || len(raw) != keyLen || raw[0] != 0x04 {
		return nil, nil, errBadKeys
	}
	pub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, nil, errBadKeys
	}
	secret, err := decodeB64(auth)
	if err != nil || len(secret) != authSecretLen {
		return nil, nil, errBadKeys
	}
	return pub, secret, nil
}

// paddedLen is the record-plaintext length for a payload of n bytes: the smallest bucket
// that holds it plus the delimiter.
func paddedLen(n int) (int, error) {
	for _, b := range paddingBuckets {
		if n+1 <= b {
			return b, nil
		}
	}
	return 0, errPayloadTooLarge
}

// encrypt produces an aes128gcm body for the receiver: header (salt, rs, keyid = the
// ephemeral public key) followed by one record holding the padded plaintext.
func encrypt(plaintext []byte, uaPublic *ecdh.PublicKey, authSecret []byte) ([]byte, error) {
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encryptWith(plaintext, uaPublic, authSecret, asPrivate, salt)
}

// encryptWith is encrypt with the ephemeral key and salt supplied - only the RFC 8291
// test vector passes its own; real sends always use fresh ones.
func encryptWith(plaintext []byte, uaPublic *ecdh.PublicKey, authSecret []byte, asPrivate *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	size, err := paddedLen(len(plaintext))
	if err != nil {
		return nil, err
	}
	cek, nonce, err := deriveKeys(asPrivate, uaPublic, asPrivate.PublicKey().Bytes(), uaPublic.Bytes(), authSecret, salt)
	if err != nil {
		return nil, err
	}
	record := make([]byte, size)
	copy(record, plaintext)
	// RFC 8188 §2: the last (here: only) record's padding delimiter is 0x02, then zeros.
	record[len(plaintext)] = 0x02

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	asPub := asPrivate.PublicKey().Bytes()
	out := make([]byte, 0, headerLen+size+tagLen)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return gcm.Seal(out, nonce, record, nil), nil
}

// deriveKeys is RFC 8291 §3.3-3.4 plus RFC 8188 §2.2-2.3: the ECDH secret is mixed with
// the receiver's auth secret into the input keying material, which with the salt yields
// the content-encryption key and the nonce. ourPriv/theirPub is whichever side runs it;
// asPub/uaPub are always the sender's and receiver's public keys, in that order.
func deriveKeys(ourPriv *ecdh.PrivateKey, theirPub *ecdh.PublicKey, asPub, uaPub, authSecret, salt []byte) (cek, nonce []byte, err error) {
	ecdhSecret, err := ourPriv.ECDH(theirPub)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := make([]byte, 0, 14+len(uaPub)+len(asPub))
	keyInfo = append(keyInfo, "WebPush: info\x00"...)
	keyInfo = append(keyInfo, uaPub...)
	keyInfo = append(keyInfo, asPub...)
	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}
