package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// KeyPair represents a Curve25519 key pair
type KeyPair struct {
	PrivateKey [32]byte
	PublicKey  [32]byte
}

// IdentityKeyPair represents an Ed25519 identity key pair
type IdentityKeyPair struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
}

// PreKeyBundle contains all the keys needed to initiate a session
type PreKeyBundle struct {
	IdentityKey    ed25519.PublicKey
	SignedPreKey   [32]byte
	Signature      []byte
	OneTimePreKey  *[32]byte // Optional
	PreKeyID       int32
	SignedPreKeyID int32
}

// SessionState represents the state of a Signal Protocol session
type SessionState struct {
	RootKey       [32]byte
	ChainKey      [32]byte
	SendingChain  ChainState
	ReceivingChain ChainState
	MessageNumber uint32
	PreviousCounter uint32
}

// ChainState represents a message chain state
type ChainState struct {
	ChainKey    [32]byte
	MessageKey  [32]byte
	Counter     uint32
}

// MessageKeys contains the keys for encrypting/decrypting a single message
type MessageKeys struct {
	CipherKey [32]byte
	MacKey    [32]byte
	IV        [16]byte
}

// GenerateKeyPair generates a new Curve25519 key pair
func GenerateKeyPair() (*KeyPair, error) {
	var privateKey [32]byte
	if _, err := rand.Read(privateKey[:]); err != nil {
		return nil, err
	}

	var publicKey [32]byte
	curve25519.ScalarBaseMult(&publicKey, &privateKey)

	return &KeyPair{
		PrivateKey: privateKey,
		PublicKey:  publicKey,
	}, nil
}

// GenerateIdentityKeyPair generates a new Ed25519 identity key pair
func GenerateIdentityKeyPair() (*IdentityKeyPair, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	return &IdentityKeyPair{
		PrivateKey: privKey,
		PublicKey:  pubKey,
	}, nil
}

// SignPreKey signs a prekey with the identity key
func (ikp *IdentityKeyPair) SignPreKey(preKey [32]byte) []byte {
	return ed25519.Sign(ikp.PrivateKey, preKey[:])
}

// VerifyPreKeySignature verifies a prekey signature
func VerifyPreKeySignature(identityKey ed25519.PublicKey, preKey [32]byte, signature []byte) bool {
	return ed25519.Verify(identityKey, preKey[:], signature)
}

// DeriveSharedSecret performs X3DH key agreement with proper HKDF
func DeriveSharedSecret(identityKey [32]byte, ephemeralKey [32]byte, preKey [32]byte, oneTimePreKey *[32]byte) ([32]byte, error) {
	var sharedSecret [32]byte

	// DH1 = DH(IK_A, SPK_B)
	dh1, err := curve25519.X25519(identityKey[:], preKey[:])
	if err != nil {
		return sharedSecret, err
	}

	// DH2 = DH(EK_A, IK_B) - We'll skip this for simplicity
	// DH3 = DH(EK_A, SPK_B)
	dh3, err := curve25519.X25519(ephemeralKey[:], preKey[:])
	if err != nil {
		return sharedSecret, err
	}

	// Combine the shared secrets
	combined := append(dh1, dh3...)

	// If one-time prekey is available, include it
	if oneTimePreKey != nil {
		dh4, err := curve25519.X25519(ephemeralKey[:], oneTimePreKey[:])
		if err != nil {
			return sharedSecret, err
		}
		combined = append(combined, dh4...)
	}

	// Use proper HKDF to derive the final shared secret
	hkdfReader := hkdf.New(sha256.New, combined, nil, []byte("Signal_X3DH"))
	_, err = io.ReadFull(hkdfReader, sharedSecret[:])
	if err != nil {
		return sharedSecret, err
	}

	return sharedSecret, nil
}

// DeriveRootKey derives the root key from the shared secret using HKDF
func DeriveRootKey(sharedSecret [32]byte) [32]byte {
	var rootKey [32]byte
	hkdfReader := hkdf.New(sha256.New, sharedSecret[:], nil, []byte("Signal_RootKey"))
	io.ReadFull(hkdfReader, rootKey[:])
	return rootKey
}

// DeriveChainKey derives a new chain key from the current chain key using HMAC
func DeriveChainKey(chainKey [32]byte) [32]byte {
	// Use HMAC-SHA256 with a constant as per Signal Protocol
	constant := []byte{0x02}
	mac := hmac.New(sha256.New, chainKey[:])
	mac.Write(constant)
	result := mac.Sum(nil)

	var newChainKey [32]byte
	copy(newChainKey[:], result)
	return newChainKey
}

// DeriveMessageKey derives a message key from the chain key using HMAC
func DeriveMessageKey(chainKey [32]byte) MessageKeys {
	// Derive cipher key using HMAC
	cipherConstant := []byte{0x01}
	cipherMac := hmac.New(sha256.New, chainKey[:])
	cipherMac.Write(cipherConstant)
	cipherResult := cipherMac.Sum(nil)

	// Derive MAC key using HMAC
	macConstant := []byte{0x02}
	macMac := hmac.New(sha256.New, chainKey[:])
	macMac.Write(macConstant)
	macResult := macMac.Sum(nil)

	// Derive IV using HMAC
	ivConstant := []byte{0x03}
	ivMac := hmac.New(sha256.New, chainKey[:])
	ivMac.Write(ivConstant)
	ivResult := ivMac.Sum(nil)

	var messageKeys MessageKeys
	copy(messageKeys.CipherKey[:], cipherResult)
	copy(messageKeys.MacKey[:], macResult)
	copy(messageKeys.IV[:], ivResult[:16])

	return messageKeys
}

// InitializeSession initializes a new Signal Protocol session
func InitializeSession(bundle *PreKeyBundle, identityKeyPair *IdentityKeyPair) (*SessionState, error) {
	// Generate ephemeral key
	ephemeralKey, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}

	// Convert identity key to Curve25519 format (simplified)
	var identityKeyCurve [32]byte
	copy(identityKeyCurve[:], identityKeyPair.PublicKey[:32])

	// Derive shared secret
	sharedSecret, err := DeriveSharedSecret(
		identityKeyCurve,
		ephemeralKey.PrivateKey,
		bundle.SignedPreKey,
		bundle.OneTimePreKey,
	)
	if err != nil {
		return nil, err
	}

	// Derive root key
	rootKey := DeriveRootKey(sharedSecret)

	// Initialize chain key
	chainKey := DeriveChainKey(rootKey)

	return &SessionState{
		RootKey:  rootKey,
		ChainKey: chainKey,
		SendingChain: ChainState{
			ChainKey: chainKey,
			Counter:  0,
		},
		ReceivingChain: ChainState{
			ChainKey: chainKey,
			Counter:  0,
		},
		MessageNumber:   0,
		PreviousCounter: 0,
	}, nil
}

// EncryptMessage encrypts a message using AES-GCM with proper authentication
func (s *SessionState) EncryptMessage(plaintext []byte) ([]byte, error) {
	// Derive message keys
	messageKeys := DeriveMessageKey(s.SendingChain.ChainKey)

	// Create AES cipher
	block, err := aes.NewCipher(messageKeys.CipherKey[:])
	if err != nil {
		return nil, err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Create message header
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], s.MessageNumber)
	binary.BigEndian.PutUint32(header[4:8], s.SendingChain.Counter)

	// Use first 12 bytes of IV for GCM nonce
	nonce := messageKeys.IV[:gcm.NonceSize()]

	// Encrypt with authenticated encryption (GCM)
	ciphertext := gcm.Seal(nil, nonce, plaintext, header)

	// Combine header, nonce, and ciphertext
	message := make([]byte, 0, len(header)+len(nonce)+len(ciphertext))
	message = append(message, header...)
	message = append(message, nonce...)
	message = append(message, ciphertext...)

	// Update chain key and counter
	s.SendingChain.ChainKey = DeriveChainKey(s.SendingChain.ChainKey)
	s.SendingChain.Counter++
	s.MessageNumber++

	return message, nil
}

// DecryptMessage decrypts a message using AES-GCM with authentication verification
func (s *SessionState) DecryptMessage(message []byte) ([]byte, error) {
	if len(message) < 20 { // 8 bytes header + 12 bytes nonce + at least some ciphertext
		return nil, errors.New("message too short")
	}

	// Extract header
	header := message[0:8]
	_ = binary.BigEndian.Uint32(header[0:4]) // messageNumber (unused in this simplified implementation)
	chainCounter := binary.BigEndian.Uint32(header[4:8])

	// Verify message order (simplified)
	if chainCounter != s.ReceivingChain.Counter {
		return nil, fmt.Errorf("out of order message: expected %d, got %d", s.ReceivingChain.Counter, chainCounter)
	}

	// Derive message keys
	messageKeys := DeriveMessageKey(s.ReceivingChain.ChainKey)

	// Create AES cipher
	block, err := aes.NewCipher(messageKeys.CipherKey[:])
	if err != nil {
		return nil, err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Extract nonce and ciphertext
	nonceSize := gcm.NonceSize()
	nonce := message[8 : 8+nonceSize]
	ciphertext := message[8+nonceSize:]

	// Decrypt and verify authentication
	plaintext, err := gcm.Open(nil, nonce, ciphertext, header)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %v", err)
	}

	// Update chain key and counter
	s.ReceivingChain.ChainKey = DeriveChainKey(s.ReceivingChain.ChainKey)
	s.ReceivingChain.Counter++

	return plaintext, nil
}

// GeneratePreKeys generates a batch of one-time prekeys
func GeneratePreKeys(count int) ([]*KeyPair, error) {
	preKeys := make([]*KeyPair, count)
	for i := 0; i < count; i++ {
		keyPair, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		preKeys[i] = keyPair
	}
	return preKeys, nil
}

// SerializeSessionState serializes the session state for storage
func (s *SessionState) Serialize() []byte {
	data := make([]byte, 0, 256)
	data = append(data, s.RootKey[:]...)
	data = append(data, s.ChainKey[:]...)
	data = append(data, s.SendingChain.ChainKey[:]...)
	data = append(data, s.ReceivingChain.ChainKey[:]...)

	// Add counters
	counterData := make([]byte, 16)
	binary.BigEndian.PutUint32(counterData[0:4], s.MessageNumber)
	binary.BigEndian.PutUint32(counterData[4:8], s.PreviousCounter)
	binary.BigEndian.PutUint32(counterData[8:12], s.SendingChain.Counter)
	binary.BigEndian.PutUint32(counterData[12:16], s.ReceivingChain.Counter)
	data = append(data, counterData...)

	return data
}

// DeserializeSessionState deserializes the session state from storage
func DeserializeSessionState(data []byte) (*SessionState, error) {
	if len(data) < 144 { // 32*4 + 16 = 144 bytes minimum
		return nil, errors.New("invalid session data length")
	}

	var session SessionState
	copy(session.RootKey[:], data[0:32])
	copy(session.ChainKey[:], data[32:64])
	copy(session.SendingChain.ChainKey[:], data[64:96])
	copy(session.ReceivingChain.ChainKey[:], data[96:128])

	session.MessageNumber = binary.BigEndian.Uint32(data[128:132])
	session.PreviousCounter = binary.BigEndian.Uint32(data[132:136])
	session.SendingChain.Counter = binary.BigEndian.Uint32(data[136:140])
	session.ReceivingChain.Counter = binary.BigEndian.Uint32(data[140:144])

	return &session, nil
}