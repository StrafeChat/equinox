package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// decryptBody is the receiver side of RFC 8291 - what the device does. It lives in the
// tests: the server only ever encrypts.
func decryptBody(t *testing.T, body []byte, uaPrivate *ecdh.PrivateKey, authSecret []byte) []byte {
	t.Helper()
	pt, err := decryptAES128GCM(body, uaPrivate, authSecret)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return pt
}

func decryptAES128GCM(body []byte, uaPrivate *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(body) < headerLen+tagLen {
		return nil, errors.New("short body")
	}
	salt := body[:saltLen]
	rs := binary.BigEndian.Uint32(body[saltLen : saltLen+4])
	idLen := int(body[saltLen+4])
	if idLen != keyLen {
		return nil, errors.New("keyid is not a P-256 point")
	}
	keyID := body[saltLen+5 : saltLen+5+idLen]
	record := body[saltLen+5+idLen:]
	if uint32(len(record)) > rs {
		return nil, errors.New("record larger than rs")
	}
	asPublic, err := ecdh.P256().NewPublicKey(keyID)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := deriveKeys(uaPrivate, asPublic, keyID, uaPrivate.PublicKey().Bytes(), authSecret, salt)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	padded, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(padded, 0x02)
	if i < 0 || strings.Trim(string(padded[i+1:]), "\x00") != "" {
		return nil, errors.New("bad padding")
	}
	return padded[:i], nil
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// RFC 8291 Appendix A: the worked example, byte for byte. Decrypting the RFC's body with
// the RFC's receiver keys must give the RFC's plaintext, and encrypting with the RFC's
// sender key and salt must give the RFC's body (the RFC example is unpadded, so this
// drives the record by hand with the same key schedule).
func TestRFC8291AppendixA(t *testing.T) {
	const (
		plaintext   = "When I grow up, I want to be a watermelon"
		asPrivB64   = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
		asPubB64    = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
		uaPrivB64   = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
		uaPubB64    = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
		saltB64     = "DGv6ra1nlYgDCS1FRnbzlw"
		authB64     = "BTBZMqHH6r4Tts7J_aSIgg"
		cekB64      = "oIhVW04MRdy2XN9CiKLxTg"
		nonceB64    = "4h_95klXJ5E_qnoN"
		bodyB64     = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
		ecdhB64     = "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"
		receiverErr = "receiver keys"
	)
	uaPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, uaPrivB64))
	if err != nil {
		t.Fatal(receiverErr, err)
	}
	if b64(uaPriv.PublicKey().Bytes()) != uaPubB64 {
		t.Fatal("receiver public key does not match the RFC")
	}
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, asPrivB64))
	if err != nil {
		t.Fatal(err)
	}
	if b64(asPriv.PublicKey().Bytes()) != asPubB64 {
		t.Fatal("sender public key does not match the RFC")
	}
	if secret, _ := asPriv.ECDH(uaPriv.PublicKey()); b64(secret) != ecdhB64 {
		t.Fatalf("ecdh_secret = %s", b64(secret))
	}
	auth := mustB64(t, authB64)
	salt := mustB64(t, saltB64)

	// Key schedule.
	cek, nonce, err := deriveKeys(asPriv, uaPriv.PublicKey(), asPriv.PublicKey().Bytes(), uaPriv.PublicKey().Bytes(), auth, salt)
	if err != nil {
		t.Fatal(err)
	}
	if b64(cek) != cekB64 || b64(nonce) != nonceB64 {
		t.Fatalf("CEK/NONCE = %s/%s, want %s/%s", b64(cek), b64(nonce), cekB64, nonceB64)
	}

	// Receiver side: the RFC's body opens to the RFC's plaintext.
	body := mustB64(t, bodyB64)
	if got := decryptBody(t, body, uaPriv, auth); string(got) != plaintext {
		t.Fatalf("decrypted %q", got)
	}

	// Sender side, unpadded like the RFC: header + Seal(plaintext || 0x02).
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	header := append(append(append([]byte{}, salt...), 0, 0, 0x10, 0, keyLen), asPriv.PublicKey().Bytes()...)
	built := gcm.Seal(header, nonce, append([]byte(plaintext), 0x02), nil)
	if b64(built) != bodyB64 {
		t.Fatalf("encrypted body differs from the RFC:\n got %s\nwant %s", b64(built), bodyB64)
	}

	// And our own encrypt with the RFC's key and salt opens with the RFC's receiver keys,
	// padded to the first bucket.
	padded, err := encryptWith([]byte(plaintext), uaPriv.PublicKey(), auth, asPriv, salt)
	if err != nil {
		t.Fatal(err)
	}
	if len(padded) != headerLen+256+tagLen {
		t.Errorf("padded body is %d bytes, want %d", len(padded), headerLen+256+tagLen)
	}
	if !bytes.Equal(padded[:headerLen], body[:headerLen]) {
		t.Error("header differs from the RFC's")
	}
	if got := decryptBody(t, padded, uaPriv, auth); string(got) != plaintext {
		t.Fatalf("round trip gave %q", got)
	}
}

// Every payload size lands in its bucket and opens again; fresh key and salt every time.
func TestEncryptRoundTripBuckets(t *testing.T) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	cases := map[int]int{0: 256, 1: 256, 255: 256, 256: 512, 511: 512, 512: 1024, 1023: 1024, 1024: 2048, 2047: 2048}
	for n, bucket := range cases {
		msg := bytes.Repeat([]byte{'x'}, n)
		body, err := encrypt(msg, uaPriv.PublicKey(), auth)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if len(body) != headerLen+bucket+tagLen {
			t.Errorf("%d bytes -> body %d, want bucket %d", n, len(body), bucket)
		}
		if got := decryptBody(t, body, uaPriv, auth); !bytes.Equal(got, msg) {
			t.Errorf("%d bytes: round trip mismatch", n)
		}
	}
	if _, err := encrypt(make([]byte, 2048), uaPriv.PublicKey(), auth); !errors.Is(err, errPayloadTooLarge) {
		t.Errorf("2048-byte payload: err = %v, want errPayloadTooLarge", err)
	}
	a, _ := encrypt([]byte("same"), uaPriv.PublicKey(), auth)
	b, _ := encrypt([]byte("same"), uaPriv.PublicKey(), auth)
	if bytes.Equal(a[:headerLen], b[:headerLen]) {
		t.Error("two pushes reused a salt or sender key")
	}
}

func TestParseReceiverKeys(t *testing.T) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	pub := b64(uaPriv.PublicKey().Bytes())
	auth := b64(make([]byte, 16))
	if _, _, err := parseReceiverKeys(pub, auth); err != nil {
		t.Fatalf("valid keys refused: %v", err)
	}
	// Padded base64url is tolerated.
	if _, _, err := parseReceiverKeys(pub+"=", auth+"=="); err != nil {
		t.Errorf("padded keys refused: %v", err)
	}
	offCurve := append([]byte{4}, bytes.Repeat([]byte{1}, 64)...)
	bad := map[string][2]string{
		"off curve":  {b64(offCurve), auth},
		"compressed": {b64(append([]byte{2}, uaPriv.PublicKey().Bytes()[1:33]...)), auth},
		"not b64":    {"!!!", auth},
		"short auth": {pub, b64(make([]byte, 8))},
		"empty":      {"", ""},
	}
	for name, k := range bad {
		if _, _, err := parseReceiverKeys(k[0], k[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
