package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/database/models"
	"github.com/scylladb/gocqlx/v2"
	"github.com/scylladb/gocqlx/v2/qb"
)

// E2EEService handles all E2EE operations
type E2EEService struct {
	session *gocqlx.Session
}

// NewE2EEService creates a new E2EE service instance
func NewE2EEService() *E2EEService {
	return &E2EEService{
		session: database.Session,
	}
}

// InitializeUserKeys initializes E2EE keys for a new user
func (s *E2EEService) InitializeUserKeys(userID string) error {
	// Generate identity key pair
	identityKeyPair, err := GenerateIdentityKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate identity key: %w", err)
	}

	// Store identity key (public key only - private key should be stored securely on client)
	identityKey := &models.E2EEIdentityKey{
		UserID:    userID,
		PublicKey: identityKeyPair.PublicKey,
		KeyType:   "ed25519",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.session.Query(models.E2EEIdentityKeyTable.Insert()).BindStruct(identityKey).ExecRelease(); err != nil {
		return fmt.Errorf("failed to store identity key: %w", err)
	}

	// Generate signed prekey
	signedPreKeyPair, err := GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate signed prekey: %w", err)
	}

	// Sign the prekey
	signature := identityKeyPair.SignPreKey(signedPreKeyPair.PublicKey)

	// Store signed prekey
	signedPreKey := &models.E2EESignedPreKey{
		UserID:    userID,
		KeyID:     1, // Start with ID 1
		PublicKey: signedPreKeyPair.PublicKey[:],
		Signature: signature,
		CreatedAt: time.Now(),
	}

	if err := s.session.Query(models.E2EESignedPreKeyTable.Insert()).BindStruct(signedPreKey).ExecRelease(); err != nil {
		return fmt.Errorf("failed to store signed prekey: %w", err)
	}

	// Generate one-time prekeys
	preKeys, err := GeneratePreKeys(100) // Generate 100 one-time prekeys
	if err != nil {
		return fmt.Errorf("failed to generate prekeys: %w", err)
	}

	// Store one-time prekeys
	for i, preKey := range preKeys {
		preKeyModel := &models.E2EEPreKey{
			UserID:    userID,
			KeyID:     int32(i + 1),
			PublicKey: preKey.PublicKey[:],
			Used:      false,
			CreatedAt: time.Now(),
		}

		if err := s.session.Query(models.E2EEPreKeyTable.Insert()).BindStruct(preKeyModel).ExecRelease(); err != nil {
			return fmt.Errorf("failed to store prekey %d: %w", i, err)
		}
	}

	log.Printf("Initialized E2EE keys for user %s", userID)
	return nil
}

// GetPreKeyBundle retrieves the prekey bundle for a user
func (s *E2EEService) GetPreKeyBundle(userID string) (*PreKeyBundle, error) {
	// Get identity key
	var identityKey models.E2EEIdentityKey
	if err := s.session.Query(models.E2EEIdentityKeyTable.Get()).BindMap(qb.M{"user_id": userID}).GetRelease(&identityKey); err != nil {
		return nil, fmt.Errorf("failed to get identity key: %w", err)
	}

	// Get signed prekey (latest one)
	var signedPreKey models.E2EESignedPreKey
	query := qb.Select("e2ee_signed_prekeys").Where(qb.Eq("user_id")).OrderBy("key_id", qb.DESC).Limit(1).Query(*s.session)
	if err := query.BindMap(qb.M{"user_id": userID}).GetRelease(&signedPreKey); err != nil {
		return nil, fmt.Errorf("failed to get signed prekey: %w", err)
	}

	// Get an unused one-time prekey
	var preKey models.E2EEPreKey
	var oneTimePreKey *[32]byte
	preKeyQuery := qb.Select("e2ee_prekeys").Where(qb.Eq("user_id"), qb.Eq("used")).Limit(1).Query(*s.session)
	if err := preKeyQuery.BindMap(qb.M{"user_id": userID, "used": false}).GetRelease(&preKey); err == nil {
		// Mark the prekey as used
		updateQuery := qb.Update("e2ee_prekeys").Set("used").Where(qb.Eq("user_id"), qb.Eq("key_id")).Query(*s.session)
		if err := updateQuery.BindMap(qb.M{"user_id": userID, "key_id": preKey.KeyID, "used": true}).ExecRelease(); err != nil {
			log.Printf("Failed to mark prekey as used: %v", err)
		}
		var key [32]byte
		copy(key[:], preKey.PublicKey)
		oneTimePreKey = &key
	}

	// Convert signed prekey to [32]byte
	var signedPreKeyBytes [32]byte
	copy(signedPreKeyBytes[:], signedPreKey.PublicKey)

	return &PreKeyBundle{
		IdentityKey:    identityKey.PublicKey,
		SignedPreKey:   signedPreKeyBytes,
		Signature:      signedPreKey.Signature,
		OneTimePreKey:  oneTimePreKey,
		PreKeyID:       preKey.KeyID,
		SignedPreKeyID: signedPreKey.KeyID,
	}, nil
}

// CreateSession creates a new E2EE session between two users
// Note: In production, the identity private key should be provided securely from the client
func (s *E2EEService) CreateSession(senderID, recipientID string, senderIdentityPrivateKey []byte) error {
	// Get sender's identity key
	var senderIdentity models.E2EEIdentityKey
	if err := s.session.Query(models.E2EEIdentityKeyTable.Get()).BindMap(qb.M{"user_id": senderID}).GetRelease(&senderIdentity); err != nil {
		return fmt.Errorf("failed to get sender identity key: %w", err)
	}

	// Get recipient's prekey bundle
	bundle, err := s.GetPreKeyBundle(recipientID)
	if err != nil {
		return fmt.Errorf("failed to get recipient prekey bundle: %w", err)
	}

	// Verify the signed prekey signature before proceeding
	if !VerifyPreKeySignature(bundle.IdentityKey, bundle.SignedPreKey, bundle.Signature) {
		return fmt.Errorf("invalid signed prekey signature for recipient %s", recipientID)
	}

	// Create identity key pair with provided private key
	identityKeyPair := &IdentityKeyPair{
		PublicKey:  senderIdentity.PublicKey,
		PrivateKey: senderIdentityPrivateKey,
	}

	// Initialize session using proper X3DH
	sessionState, err := InitializeSession(bundle, identityKeyPair)
	if err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}

	// Serialize and store session
	sessionData, err := sessionState.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize session: %w", err)
	}

	session := &models.E2EESession{
		UserID:          senderID,
		RecipientID:     recipientID,
		SessionData:     sessionData,
		SessionVersion:  1,
		ProtocolVersion: "signal_v1",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	if err := s.session.Query(models.E2EESessionTable.Insert()).BindStruct(session).ExecRelease(); err != nil {
		return fmt.Errorf("failed to store session: %w", err)
	}

	log.Printf("Created E2EE session between %s and %s using X3DH", senderID, recipientID)
	return nil
}

// EncryptMessage encrypts a message for a specific recipient using Double Ratchet
func (s *E2EEService) EncryptMessage(senderID, recipientID, plaintext string) (string, error) {
	// Get session
	var sessionModel models.E2EESession
	if err := s.session.Query(models.E2EESessionTable.Get()).BindMap(qb.M{"user_id": senderID, "recipient_id": recipientID}).GetRelease(&sessionModel); err != nil {
		return "", fmt.Errorf("no session found between %s and %s. Session must be created first", senderID, recipientID)
	}

	// Deserialize session state
	sessionState, err := DeserializeSessionState(sessionModel.SessionData)
	if err != nil {
		return "", fmt.Errorf("failed to deserialize session: %w", err)
	}

	// Encrypt message
	ciphertext, err := sessionState.EncryptMessage([]byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("failed to encrypt message: %w", err)
	}

	// Update session state in database with version increment
	updatedSessionData, err := sessionState.Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to serialize updated session: %w", err)
	}

	updateQuery := qb.Update("e2ee_sessions").Set("session_data", "session_version", "updated_at").Where(qb.Eq("user_id"), qb.Eq("recipient_id")).Query(*s.session)
	if err := updateQuery.BindMap(qb.M{
		"user_id":        senderID,
		"recipient_id":   recipientID,
		"session_data":   updatedSessionData,
		"session_version": sessionModel.SessionVersion + 1,
		"updated_at":     time.Now(),
	}).ExecRelease(); err != nil {
		return "", fmt.Errorf("failed to update session: %w", err)
	}

	// Return encrypted message as string (binary data)
	return string(ciphertext), nil
}

// DecryptMessage decrypts a message from a specific sender
func (s *E2EEService) DecryptMessage(recipientID, senderID, ciphertext string) (string, error) {
	// Get session
	var sessionModel models.E2EESession
	if err := s.session.Query(models.E2EESessionTable.Get()).BindMap(qb.M{"user_id": recipientID, "recipient_id": senderID}).GetRelease(&sessionModel); err != nil {
		return "", fmt.Errorf("failed to get session: %w", err)
	}

	// Deserialize session state
	sessionState, err := DeserializeSessionState(sessionModel.SessionData)
	if err != nil {
		return "", fmt.Errorf("failed to deserialize session: %w", err)
	}

	// Decrypt message
	plaintext, err := sessionState.DecryptMessage([]byte(ciphertext))
	if err != nil {
		return "", fmt.Errorf("failed to decrypt message: %w", err)
	}

	// Update session state in database with version increment
	updatedSessionData, err := sessionState.Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to serialize updated session: %w", err)
	}

	updateQuery := qb.Update("e2ee_sessions").Set("session_data", "session_version", "updated_at").Where(qb.Eq("user_id"), qb.Eq("recipient_id")).Query(*s.session)
	if err := updateQuery.BindMap(qb.M{
		"user_id":        recipientID,
		"recipient_id":   senderID,
		"session_data":   updatedSessionData,
		"session_version": sessionModel.SessionVersion + 1,
		"updated_at":     time.Now(),
	}).ExecRelease(); err != nil {
		return "", fmt.Errorf("failed to update session: %w", err)
	}

	return string(plaintext), nil
}

// CreateGroupSession creates a new group session for a room
func (s *E2EEService) CreateGroupSession(roomID, creatorID string, participants []string) error {
	// Generate session key
	sessionKey := make([]byte, 32)
	if _, err := rand.Read(sessionKey); err != nil {
		return fmt.Errorf("failed to generate session key: %w", err)
	}

	// Generate session ID
	sessionID := fmt.Sprintf("%d", time.Now().UnixNano())

	// Create group session
	groupSession := &models.E2EEGroupSession{
		RoomID:       roomID,
		SessionID:    sessionID,
		SessionKey:   sessionKey,
		CreatorID:    creatorID,
		Participants: participants,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := s.session.Query(models.E2EEGroupSessionTable.Insert()).BindStruct(groupSession).ExecRelease(); err != nil {
		return fmt.Errorf("failed to store group session: %w", err)
	}

	log.Printf("Created group session %s for room %s", sessionID, roomID)
	return nil
}

// EncryptGroupMessage encrypts a message for a group using AES-GCM
func (s *E2EEService) EncryptGroupMessage(roomID, senderID, plaintext string) (string, error) {
	// Get latest group session
	var groupSession models.E2EEGroupSession
	query := qb.Select("e2ee_group_sessions").Where(qb.Eq("room_id")).OrderBy("session_id", qb.DESC).Limit(1).Query(*s.session)
	if err := query.BindMap(qb.M{"room_id": roomID}).GetRelease(&groupSession); err != nil {
		return "", fmt.Errorf("failed to get group session: %w", err)
	}

	// Use proper AES-GCM encryption
	block, err := aes.NewCipher(groupSession.SessionKey)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Create additional authenticated data
	aad := []byte(fmt.Sprintf("%s:%s", roomID, senderID))

	// Encrypt the message
	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), aad)

	// Combine nonce, AAD length, AAD, and ciphertext
	result := make([]byte, 0, len(nonce)+4+len(aad)+len(ciphertext))
	result = append(result, nonce...)
	
	// Add AAD length (4 bytes)
	aadLen := make([]byte, 4)
	binary.BigEndian.PutUint32(aadLen, uint32(len(aad)))
	result = append(result, aadLen...)
	
	// Add AAD and ciphertext
	result = append(result, aad...)
	result = append(result, ciphertext...)

	return string(result), nil
}

// DecryptGroupMessage decrypts a group message using AES-GCM
func (s *E2EEService) DecryptGroupMessage(roomID, ciphertext string) (string, error) {
	// Get latest group session
	var groupSession models.E2EEGroupSession
	query := qb.Select("e2ee_group_sessions").Where(qb.Eq("room_id")).OrderBy("session_id", qb.DESC).Limit(1).Query(*s.session)
	if err := query.BindMap(qb.M{"room_id": roomID}).GetRelease(&groupSession); err != nil {
		return "", fmt.Errorf("failed to get group session: %w", err)
	}

	ciphertextBytes := []byte(ciphertext)
	
	// Check if this is legacy XOR encryption (backward compatibility)
	if len(ciphertextBytes) < 16 { // Too short for GCM format
		// Legacy XOR decryption
		plaintext := make([]byte, len(ciphertextBytes))
		for i := range ciphertextBytes {
			plaintext[i] = ciphertextBytes[i] ^ groupSession.SessionKey[i%len(groupSession.SessionKey)]
		}
		return string(plaintext), nil
	}

	// Use proper AES-GCM decryption
	block, err := aes.NewCipher(groupSession.SessionKey)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Extract components
	nonceSize := gcm.NonceSize()
	if len(ciphertextBytes) < nonceSize+4 {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce := ciphertextBytes[:nonceSize]
	aadLen := binary.BigEndian.Uint32(ciphertextBytes[nonceSize : nonceSize+4])
	
	if len(ciphertextBytes) < nonceSize+4+int(aadLen) {
		return "", fmt.Errorf("ciphertext too short for AAD")
	}

	aad := ciphertextBytes[nonceSize+4 : nonceSize+4+int(aadLen)]
	encryptedData := ciphertextBytes[nonceSize+4+int(aadLen):]

	// Decrypt and verify authentication
	plaintext, err := gcm.Open(nil, nonce, encryptedData, aad)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

// RefreshPreKeys generates new one-time prekeys when running low
func (s *E2EEService) RefreshPreKeys(userID string, count int) error {
	// Generate new prekeys
	preKeys, err := GeneratePreKeys(count)
	if err != nil {
		return fmt.Errorf("failed to generate prekeys: %w", err)
	}

	// Get the highest existing key ID
	var maxKeyID int32
	query := s.session.Query("SELECT MAX(key_id) FROM e2ee_prekeys WHERE user_id = ?", []string{})
	if err := query.Bind(userID).Scan(&maxKeyID); err != nil {
		maxKeyID = 0 // Start from 0 if no keys exist
	}

	// Store new prekeys
	for i, preKey := range preKeys {
		preKeyModel := &models.E2EEPreKey{
			UserID:    userID,
			KeyID:     maxKeyID + int32(i) + 1,
			PublicKey: preKey.PublicKey[:],
			Used:      false,
			CreatedAt: time.Now(),
		}

		if err := s.session.Query(models.E2EEPreKeyTable.Insert()).BindStruct(preKeyModel).ExecRelease(); err != nil {
			return fmt.Errorf("failed to store prekey %d: %w", i, err)
		}
	}

	log.Printf("Refreshed %d prekeys for user %s", count, userID)
	return nil
}

// GetUserE2EEStatus checks if a user has E2EE keys initialized
func (s *E2EEService) GetUserE2EEStatus(userID string) (bool, error) {
	var identityKey models.E2EEIdentityKey
	err := s.session.Query(models.E2EEIdentityKeyTable.Get()).BindMap(qb.M{"user_id": userID}).GetRelease(&identityKey)
	if err != nil {
		return false, nil // User doesn't have E2EE keys
	}
	return true, nil
}
