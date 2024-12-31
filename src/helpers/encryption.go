package helpers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"github.com/joho/godotenv"
)

var key []byte

func init() {

	err := godotenv.Load(".env")
	if err != nil {
		panic("Error while loading enviroment variables: " + err.Error())
	}

	key = []byte(os.Getenv("ENCRYPTION_KEY"))
	if len(key) == 0 {
		panic("ENCRYPTION_KEY environment variable is not set.")
	}
	if len(key) != 16 && len(key) != 24 && len(key) != 32 {
		panic("ENCRYPTION_KEY must be 16, 24, or 32 bytes long.")
	}
}

func Encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func Decrypt(encryptedtext string) ([]byte, error) {
	cipherText, err := base64.StdEncoding.DecodeString(encryptedtext)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(cipherText) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext is too short")
	}

	nonce, ciphertext := cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
