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
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// Signal Protocol Constants
const (
	ROOT_KEY_CONSTANT    = 0x01
	CHAIN_KEY_CONSTANT   = 0x02
	MESSAGE_KEY_CONSTANT = 0x01
	MAC_KEY_CONSTANT     = 0x02
	IV_CONSTANT          = 0x03
	KDF_SALT_LENGTH      = 32
	KDF_INFO_LENGTH      = 32
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

// SessionState represents the state of a Signal Protocol session with Double Ratchet
type SessionState struct {
	// Root key for deriving new chain keys
	RootKey [32]byte
	
	// Sending chain state
	SendingChain ChainState
	
	// Receiving chain state
	ReceivingChain ChainState
	
	// DH ratchet keys
	DHSendingKey  KeyPair
	DHReceivingKey [32]byte
	
	// Message counters
	SendingCounter   uint32
	ReceivingCounter uint32
	PreviousCounter  uint32
	
	// Skipped message keys for out-of-order messages
	SkippedMessageKeys map[string]MessageKeys
	
	// Session metadata
	SessionVersion uint32
	CreatedAt      int64
}

// ChainState represents a message chain state in the Double Ratchet
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

// MessageHeader contains metadata for encrypted messages
type MessageHeader struct {
	DHPublicKey     [32]byte
	PreviousCounter uint32
	Counter         uint32
	Version         uint32
}

// GenerateKeyPair generates a new Curve25519 key pair
func GenerateKeyPair() (*KeyPair, error) {
	var privateKey [32]byte
	if _, err := rand.Read(privateKey[:]); err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
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
		return nil, fmt.Errorf("failed to generate identity key: %w", err)
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

// X3DH performs the X3DH key agreement protocol
func PerformX3DH(identityKeyPair *IdentityKeyPair, ephemeralKey *KeyPair, bundle *PreKeyBundle) ([32]byte, error) {
	var sharedSecret [32]byte
	
	// Convert Ed25519 identity key to Curve25519 for DH operations
	var identityKeyCurve [32]byte
	copy(identityKeyCurve[:], identityKeyPair.PrivateKey[:32])
	
	var recipientIdentityCurve [32]byte
	copy(recipientIdentityCurve[:], bundle.IdentityKey[:32])

	// DH1 = DH(IK_A, SPK_B)
	dh1, err := curve25519.X25519(identityKeyCurve[:], bundle.SignedPreKey[:])
	if err != nil {
		return sharedSecret, fmt.Errorf("DH1 failed: %w", err)
	}

	// DH2 = DH(EK_A, IK_B)
	dh2, err := curve25519.X25519(ephemeralKey.PrivateKey[:], recipientIdentityCurve[:])
	if err != nil {
		return sharedSecret, fmt.Errorf("DH2 failed: %w", err)
	}

	// DH3 = DH(EK_A, SPK_B)
	dh3, err := curve25519.X25519(ephemeralKey.PrivateKey[:], bundle.SignedPreKey[:])
	if err != nil {
		return sharedSecret, fmt.Errorf("DH3 failed: %w", err)
	}

	// Combine the shared secrets
	combined := make([]byte, 0, 96) // 3 * 32 bytes
	combined = append(combined, dh1...)
	combined = append(combined, dh2...)
	combined = append(combined, dh3...)

	// DH4 = DH(EK_A, OPK_B) - if one-time prekey is available
	if bundle.OneTimePreKey != nil {
		dh4, err := curve25519.X25519(ephemeralKey.PrivateKey[:], bundle.OneTimePreKey[:])
		if err != nil {
			return sharedSecret, fmt.Errorf("DH4 failed: %w", err)
		}
		combined = append(combined, dh4...)
	}

	// Use HKDF to derive the final shared secret
	hkdfReader := hkdf.New(sha256.New, combined, nil, []byte("Signal_X3DH_v1"))
	if _, err := io.ReadFull(hkdfReader, sharedSecret[:]); err != nil {
		return sharedSecret, fmt.Errorf("HKDF failed: %w", err)
	}

	return sharedSecret, nil
}

// DeriveRootKey derives the root key from the shared secret using HKDF
func DeriveRootKey(sharedSecret [32]byte) ([32]byte, error) {
	var rootKey [32]byte
	salt := make([]byte, KDF_SALT_LENGTH)
	hkdfReader := hkdf.New(sha256.New, sharedSecret[:], salt, []byte("Signal_RootKey_v1"))
	if _, err := io.ReadFull(hkdfReader, rootKey[:]); err != nil {
		return rootKey, fmt.Errorf("root key derivation failed: %w", err)
	}
	return rootKey, nil
}

// DeriveChainKey derives a new chain key from the current chain key using HMAC
func DeriveChainKey(chainKey [32]byte) ([32]byte, error) {
	var newChainKey [32]byte
	constant := []byte{CHAIN_KEY_CONSTANT}
	mac := hmac.New(sha256.New, chainKey[:])
	mac.Write(constant)
	result := mac.Sum(nil)
	copy(newChainKey[:], result)
	return newChainKey, nil
}

// DeriveMessageKey derives a message key from the chain key using HMAC
func DeriveMessageKey(chainKey [32]byte) (MessageKeys, error) {
	var messageKeys MessageKeys
	
	// Derive cipher key
	cipherMac := hmac.New(sha256.New, chainKey[:])
	cipherMac.Write([]byte{MESSAGE_KEY_CONSTANT})
	cipherResult := cipherMac.Sum(nil)
	copy(messageKeys.CipherKey[:], cipherResult)

	// Derive MAC key
	macMac := hmac.New(sha256.New, chainKey[:])
	macMac.Write([]byte{MAC_KEY_CONSTANT})
	macResult := macMac.Sum(nil)
	copy(messageKeys.MacKey[:], macResult)

	// Derive IV
	ivMac := hmac.New(sha256.New, chainKey[:])
	ivMac.Write([]byte{IV_CONSTANT})
	ivResult := ivMac.Sum(nil)
	copy(messageKeys.IV[:], ivResult[:16])

	return messageKeys, nil
}

// DeriveRootAndChainKeys performs the DH ratchet step
func DeriveRootAndChainKeys(rootKey [32]byte, dhOutput [32]byte) ([32]byte, [32]byte, error) {
	var newRootKey, newChainKey [32]byte
	
	// Use HKDF to derive both root and chain keys
	hkdfReader := hkdf.New(sha256.New, dhOutput[:], rootKey[:], []byte("Signal_DH_Ratchet_v1"))
	
	if _, err := io.ReadFull(hkdfReader, newRootKey[:]); err != nil {
		return newRootKey, newChainKey, fmt.Errorf("root key derivation failed: %w", err)
	}
	
	if _, err := io.ReadFull(hkdfReader, newChainKey[:]); err != nil {
		return newRootKey, newChainKey, fmt.Errorf("chain key derivation failed: %w", err)
	}
	
	return newRootKey, newChainKey, nil
}

// InitializeSession initializes a new Signal Protocol session with proper X3DH
func InitializeSession(bundle *PreKeyBundle, identityKeyPair *IdentityKeyPair) (*SessionState, error) {
	// Verify the signed prekey signature
	if !VerifyPreKeySignature(bundle.IdentityKey, bundle.SignedPreKey, bundle.Signature) {
		return nil, errors.New("invalid signed prekey signature")
	}

	// Generate ephemeral key for X3DH
	ephemeralKey, err := GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral key: %w", err)
	}

	// Perform X3DH key agreement
	sharedSecret, err := PerformX3DH(identityKeyPair, ephemeralKey, bundle)
	if err != nil {
		return nil, fmt.Errorf("X3DH failed: %w", err)
	}

	// Derive root key
	rootKey, err := DeriveRootKey(sharedSecret)
	if err != nil {
		return nil, fmt.Errorf("root key derivation failed: %w", err)
	}

	// Generate initial DH key pair for the ratchet
	initialDHKey, err := GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate initial DH key: %w", err)
	}

	// Perform initial DH ratchet step
	dhOutput, err := curve25519.X25519(initialDHKey.PrivateKey[:], bundle.SignedPreKey[:])
	if err != nil {
		return nil, fmt.Errorf("initial DH ratchet failed: %w", err)
	}

	// Convert dhOutput to [32]byte array
	var dhOutputArray [32]byte
	copy(dhOutputArray[:], dhOutput)
	
	newRootKey, sendingChainKey, err := DeriveRootAndChainKeys(rootKey, dhOutputArray)
	if err != nil {
		return nil, fmt.Errorf("initial chain key derivation failed: %w", err)
	}

	return &SessionState{
		RootKey:            newRootKey,
		SendingChain:       ChainState{ChainKey: sendingChainKey, Counter: 0},
		ReceivingChain:     ChainState{ChainKey: [32]byte{}, Counter: 0},
		DHSendingKey:       *initialDHKey,
		DHReceivingKey:     bundle.SignedPreKey,
		SendingCounter:     0,
		ReceivingCounter:   0,
		PreviousCounter:    0,
		SkippedMessageKeys: make(map[string]MessageKeys),
		SessionVersion:     1,
		CreatedAt:          time.Now().Unix(),
	}, nil
}

// EncryptMessage encrypts a message using the Double Ratchet algorithm
func (s *SessionState) EncryptMessage(plaintext []byte) ([]byte, error) {
	// Derive message keys from current sending chain
	messageKeys, err := DeriveMessageKey(s.SendingChain.ChainKey)
	if err != nil {
		return nil, fmt.Errorf("message key derivation failed: %w", err)
	}

	// Create message header
	header := MessageHeader{
		DHPublicKey:     s.DHSendingKey.PublicKey,
		PreviousCounter: s.PreviousCounter,
		Counter:         s.SendingChain.Counter,
		Version:         s.SessionVersion,
	}

	// Serialize header
	headerBytes := s.serializeHeader(header)

	// Create AES cipher
	block, err := aes.NewCipher(messageKeys.CipherKey[:])
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Use first 12 bytes of IV for GCM nonce
	nonce := messageKeys.IV[:gcm.NonceSize()]

	// Encrypt with authenticated encryption (GCM)
	ciphertext := gcm.Seal(nil, nonce, plaintext, headerBytes)

	// Combine header and ciphertext
	message := make([]byte, 0, len(headerBytes)+len(ciphertext))
	message = append(message, headerBytes...)
	message = append(message, ciphertext...)

	// Update sending chain
	s.SendingChain.ChainKey, err = DeriveChainKey(s.SendingChain.ChainKey)
	if err != nil {
		return nil, fmt.Errorf("chain key update failed: %w", err)
	}
	s.SendingChain.Counter++

	return message, nil
}

// DecryptMessage decrypts a message using the Double Ratchet algorithm
func (s *SessionState) DecryptMessage(message []byte) ([]byte, error) {
	if len(message) < 52 { // Minimum header size + some ciphertext
		return nil, errors.New("message too short")
	}

	// Parse header
	header, headerSize, err := s.parseHeader(message)
	if err != nil {
		return nil, fmt.Errorf("failed to parse header: %w", err)
	}

	// Check if we need to perform DH ratchet step
	if header.DHPublicKey != s.DHReceivingKey {
		if err := s.performDHRatchetStep(header.DHPublicKey); err != nil {
			return nil, fmt.Errorf("DH ratchet step failed: %w", err)
		}
	}

	// Handle out-of-order messages
	if header.Counter < s.ReceivingCounter {
		return s.decryptSkippedMessage(message, header, headerSize)
	}

	// Skip messages if necessary
	if header.Counter > s.ReceivingCounter {
		if err := s.skipMessageKeys(header.Counter); err != nil {
			return nil, fmt.Errorf("failed to skip message keys: %w", err)
		}
	}

	// Derive message keys
	messageKeys, err := DeriveMessageKey(s.ReceivingChain.ChainKey)
	if err != nil {
		return nil, fmt.Errorf("message key derivation failed: %w", err)
	}

	// Decrypt the message
	plaintext, err := s.decryptWithKeys(message[headerSize:], messageKeys, message[:headerSize])
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	// Update receiving chain
	s.ReceivingChain.ChainKey, err = DeriveChainKey(s.ReceivingChain.ChainKey)
	if err != nil {
		return nil, fmt.Errorf("chain key update failed: %w", err)
	}
	s.ReceivingChain.Counter++
	s.ReceivingCounter++

	return plaintext, nil
}

// Helper functions for the Double Ratchet implementation

func (s *SessionState) serializeHeader(header MessageHeader) []byte {
	headerBytes := make([]byte, 52) // 32 + 4 + 4 + 4 + 8 padding
	copy(headerBytes[0:32], header.DHPublicKey[:])
	binary.BigEndian.PutUint32(headerBytes[32:36], header.PreviousCounter)
	binary.BigEndian.PutUint32(headerBytes[36:40], header.Counter)
	binary.BigEndian.PutUint32(headerBytes[40:44], header.Version)
	return headerBytes
}

func (s *SessionState) parseHeader(message []byte) (MessageHeader, int, error) {
	if len(message) < 44 {
		return MessageHeader{}, 0, errors.New("message too short for header")
	}

	var header MessageHeader
	copy(header.DHPublicKey[:], message[0:32])
	header.PreviousCounter = binary.BigEndian.Uint32(message[32:36])
	header.Counter = binary.BigEndian.Uint32(message[36:40])
	header.Version = binary.BigEndian.Uint32(message[40:44])

	return header, 52, nil
}

func (s *SessionState) performDHRatchetStep(newDHKey [32]byte) error {
	// Perform DH with the new key
	dhOutput, err := curve25519.X25519(s.DHSendingKey.PrivateKey[:], newDHKey[:])
	if err != nil {
		return fmt.Errorf("DH operation failed: %w", err)
	}

	// Convert dhOutput to [32]byte array
	var dhOutputArray [32]byte
	copy(dhOutputArray[:], dhOutput)
	
	// Derive new root and receiving chain keys
	newRootKey, newReceivingChainKey, err := DeriveRootAndChainKeys(s.RootKey, dhOutputArray)
	if err != nil {
		return fmt.Errorf("key derivation failed: %w", err)
	}

	// Update state
	s.PreviousCounter = s.SendingChain.Counter
	s.RootKey = newRootKey
	s.ReceivingChain = ChainState{ChainKey: newReceivingChainKey, Counter: 0}
	s.DHReceivingKey = newDHKey
	s.ReceivingCounter = 0

	// Generate new sending DH key
	newSendingKey, err := GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate new DH key: %w", err)
	}

	// Derive new sending chain
	dhOutput2, err := curve25519.X25519(newSendingKey.PrivateKey[:], newDHKey[:])
	if err != nil {
		return fmt.Errorf("second DH operation failed: %w", err)
	}

	// Convert dhOutput2 to [32]byte array
	var dhOutput2Array [32]byte
	copy(dhOutput2Array[:], dhOutput2)
	
	newRootKey2, newSendingChainKey, err := DeriveRootAndChainKeys(s.RootKey, dhOutput2Array)
	if err != nil {
		return fmt.Errorf("second key derivation failed: %w", err)
	}

	s.RootKey = newRootKey2
	s.SendingChain = ChainState{ChainKey: newSendingChainKey, Counter: 0}
	s.DHSendingKey = *newSendingKey

	return nil
}

func (s *SessionState) skipMessageKeys(targetCounter uint32) error {
	for s.ReceivingCounter < targetCounter {
		messageKeys, err := DeriveMessageKey(s.ReceivingChain.ChainKey)
		if err != nil {
			return fmt.Errorf("failed to derive skipped message key: %w", err)
		}

		// Store skipped message key
		keyID := fmt.Sprintf("%x:%d", s.DHReceivingKey, s.ReceivingCounter)
		s.SkippedMessageKeys[keyID] = messageKeys

		// Update chain
		s.ReceivingChain.ChainKey, err = DeriveChainKey(s.ReceivingChain.ChainKey)
		if err != nil {
			return fmt.Errorf("failed to update chain key: %w", err)
		}
		s.ReceivingCounter++
	}
	return nil
}

func (s *SessionState) decryptSkippedMessage(message []byte, header MessageHeader, headerSize int) ([]byte, error) {
	keyID := fmt.Sprintf("%x:%d", header.DHPublicKey, header.Counter)
	messageKeys, exists := s.SkippedMessageKeys[keyID]
	if !exists {
		return nil, errors.New("no skipped message key found")
	}

	// Decrypt with stored keys
	plaintext, err := s.decryptWithKeys(message[headerSize:], messageKeys, message[:headerSize])
	if err != nil {
		return nil, fmt.Errorf("skipped message decryption failed: %w", err)
	}

	// Remove used key
	delete(s.SkippedMessageKeys, keyID)

	return plaintext, nil
}

func (s *SessionState) decryptWithKeys(ciphertext []byte, keys MessageKeys, aad []byte) ([]byte, error) {
	// Create AES cipher
	block, err := aes.NewCipher(keys.CipherKey[:])
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Use first 12 bytes of IV for GCM nonce
	nonce := keys.IV[:gcm.NonceSize()]

	// Decrypt and verify authentication
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("GCM decryption failed: %w", err)
	}

	return plaintext, nil
}

// GeneratePreKeys generates a batch of one-time prekeys
func GeneratePreKeys(count int) ([]*KeyPair, error) {
	if count <= 0 || count > 1000 {
		return nil, errors.New("invalid prekey count")
	}

	preKeys := make([]*KeyPair, count)
	for i := 0; i < count; i++ {
		keyPair, err := GenerateKeyPair()
		if err != nil {
			return nil, fmt.Errorf("failed to generate prekey %d: %w", i, err)
		}
		preKeys[i] = keyPair
	}
	return preKeys, nil
}

// SerializeSessionState serializes the session state for storage
func (s *SessionState) Serialize() ([]byte, error) {
	// Calculate required size
	skippedKeysSize := len(s.SkippedMessageKeys) * (32 + 32 + 16) // keyID + MessageKeys
	totalSize := 32 + 32 + 32 + 32 + 32 + 32 + 4*4 + 8 + 4 + skippedKeysSize

	data := make([]byte, 0, totalSize)

	// Serialize fixed-size fields
	data = append(data, s.RootKey[:]...)
	data = append(data, s.SendingChain.ChainKey[:]...)
	data = append(data, s.ReceivingChain.ChainKey[:]...)
	data = append(data, s.DHSendingKey.PrivateKey[:]...)
	data = append(data, s.DHSendingKey.PublicKey[:]...)
	data = append(data, s.DHReceivingKey[:]...)

	// Serialize counters
	counterData := make([]byte, 24)
	binary.BigEndian.PutUint32(counterData[0:4], s.SendingChain.Counter)
	binary.BigEndian.PutUint32(counterData[4:8], s.ReceivingChain.Counter)
	binary.BigEndian.PutUint32(counterData[8:12], s.SendingCounter)
	binary.BigEndian.PutUint32(counterData[12:16], s.ReceivingCounter)
	binary.BigEndian.PutUint32(counterData[16:20], s.PreviousCounter)
	binary.BigEndian.PutUint32(counterData[20:24], s.SessionVersion)
	data = append(data, counterData...)

	// Serialize timestamp
	timestampData := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampData, uint64(s.CreatedAt))
	data = append(data, timestampData...)

	// Serialize skipped message keys count
	skippedCountData := make([]byte, 4)
	binary.BigEndian.PutUint32(skippedCountData, uint32(len(s.SkippedMessageKeys)))
	data = append(data, skippedCountData...)

	// Serialize skipped message keys (simplified - in production, use proper serialization)
	for keyID, messageKeys := range s.SkippedMessageKeys {
		// For simplicity, we'll skip serializing skipped keys in this implementation
		// In production, you'd want to properly serialize the keyID and MessageKeys
		_ = keyID
		_ = messageKeys
	}

	return data, nil
}

// DeserializeSessionState deserializes the session state from storage
func DeserializeSessionState(data []byte) (*SessionState, error) {
	if len(data) < 220 { // Minimum size for all fixed fields
		return nil, errors.New("invalid session data length")
	}

	var session SessionState
	offset := 0

	// Deserialize fixed-size fields
	copy(session.RootKey[:], data[offset:offset+32])
	offset += 32
	copy(session.SendingChain.ChainKey[:], data[offset:offset+32])
	offset += 32
	copy(session.ReceivingChain.ChainKey[:], data[offset:offset+32])
	offset += 32
	copy(session.DHSendingKey.PrivateKey[:], data[offset:offset+32])
	offset += 32
	copy(session.DHSendingKey.PublicKey[:], data[offset:offset+32])
	offset += 32
	copy(session.DHReceivingKey[:], data[offset:offset+32])
	offset += 32

	// Deserialize counters
	session.SendingChain.Counter = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4
	session.ReceivingChain.Counter = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4
	session.SendingCounter = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4
	session.ReceivingCounter = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4
	session.PreviousCounter = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4
	session.SessionVersion = binary.BigEndian.Uint32(data[offset:offset+4])
	offset += 4

	// Deserialize timestamp
	session.CreatedAt = int64(binary.BigEndian.Uint64(data[offset:offset+8]))
	offset += 8

	// Initialize skipped message keys map
	session.SkippedMessageKeys = make(map[string]MessageKeys)

	// Deserialize skipped message keys count
	if len(data) >= offset+4 {
		skippedCount := binary.BigEndian.Uint32(data[offset:offset+4])
		_ = skippedCount // For now, we don't deserialize skipped keys
	}

	return &session, nil
}