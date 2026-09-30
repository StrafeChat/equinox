package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

var errTOTPKeyInvalid = errors.New("totp encryption key is not configured correctly")

// encryptTOTPSecret AES-256-GCM encrypts a TOTP secret under the instance's
// TOTP_ENCRYPTION_KEY, returning base64(nonce || ciphertext). Unlike a password hash this
// must be reversible - the server computes the code to compare - so it is encrypted rather
// than hashed; config.validate refuses to boot without a well-formed 32-byte key.
func encryptTOTPSecret(hexKey, plaintext string) (string, error) {
	gcm, err := totpCipher(hexKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func decryptTOTPSecret(hexKey, encoded string) (string, error) {
	gcm, err := totpCipher(hexKey)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("totp ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func totpCipher(hexKey string) (cipher.AEAD, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return nil, errTOTPKeyInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// recoveryCodeAlphabet excludes 0/O and 1/I, which a person reading a printed code back
// aloud (or typing it on a phone) routinely confuses.
const recoveryCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

const (
	recoveryCodeCount  = 10
	recoveryCodeLength = 10
)

// generateRecoveryCodes returns recoveryCodeCount single-use codes, formatted "xxxxx-xxxxx".
func generateRecoveryCodes() ([]string, error) {
	codes := make([]string, recoveryCodeCount)
	buf := make([]byte, recoveryCodeLength)
	for i := range codes {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		out := make([]byte, recoveryCodeLength)
		for j, v := range buf {
			out[j] = recoveryCodeAlphabet[int(v)%len(recoveryCodeAlphabet)]
		}
		codes[i] = string(out[:5]) + "-" + string(out[5:])
	}
	return codes, nil
}

// normalizeRecoveryCode uppercases and reinserts the canonical dash regardless of how the
// user typed or pasted it, so hashing is consistent either way.
func normalizeRecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.NewReplacer("-", "", " ", "").Replace(code)
	if len(code) != recoveryCodeLength {
		return code
	}
	return code[:5] + "-" + code[5:]
}

func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}
