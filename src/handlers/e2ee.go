package handlers

import (
	"log"
	"strconv"

	"github.com/StrafeChat/equinox/src/e2ee"
	"github.com/StrafeChat/equinox/src/utils"
	"github.com/gofiber/fiber/v3"
)

// E2EE API request/response structures
type InitializeE2EERequest struct {
	UserID string `json:"user_id"`
}

type PreKeyBundleResponse struct {
	IdentityKey    []byte `json:"identity_key"`
	SignedPreKey   []byte `json:"signed_prekey"`
	Signature      []byte `json:"signature"`
	OneTimePreKey  []byte `json:"onetime_prekey,omitempty"`
	PreKeyID       int32  `json:"prekey_id"`
	SignedPreKeyID int32  `json:"signed_prekey_id"`
}

type EncryptMessageRequest struct {
	RecipientID string `json:"recipient_id"`
	Plaintext   string `json:"plaintext"`
}

type EncryptMessageResponse struct {
	Ciphertext string `json:"ciphertext"`
	Encrypted  bool   `json:"encrypted"`
}

type DecryptMessageRequest struct {
	SenderID   string `json:"sender_id"`
	Ciphertext string `json:"ciphertext"`
}

type DecryptMessageResponse struct {
	Plaintext string `json:"plaintext"`
	Decrypted bool   `json:"decrypted"`
}

type CreateGroupSessionRequest struct {
	RoomID       string   `json:"room_id"`
	Participants []string `json:"participants"`
}

type E2EEStatusResponse struct {
	Enabled bool `json:"enabled"`
	UserID  string `json:"user_id"`
}

var e2eeService *e2ee.E2EEService

// getE2EEService returns the E2EE service, initializing it if necessary
func getE2EEService() *e2ee.E2EEService {
	if e2eeService == nil {
		e2eeService = e2ee.NewE2EEService()
	}
	return e2eeService
}

// ClientInitializeE2EERequest represents the client-side E2EE initialization request
type ClientInitializeE2EERequest struct {
	IdentityKey   []int `json:"identity_key"`
	SignedPreKey  struct {
		KeyID     int   `json:"key_id"`
		PublicKey []int `json:"public_key"`
		Signature []int `json:"signature"`
	} `json:"signed_pre_key"`
	PreKeys []struct {
		KeyID     int   `json:"key_id"`
		PublicKey []int `json:"public_key"`
	} `json:"pre_keys"`
}

// InitializeE2EE initializes E2EE keys for a user
func InitializeE2EE(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Check if the request contains client-generated keys
	var clientRequest ClientInitializeE2EERequest
	if err := c.Bind().JSON(&clientRequest); err == nil && len(clientRequest.IdentityKey) > 0 {
		// Client is providing keys, use them instead of generating on server
		log.Printf("Initializing E2EE with client-provided keys for user %s", user.ID)
		
		// Convert client-provided keys to byte arrays
		identityKey := make([]byte, len(clientRequest.IdentityKey))
		for i, v := range clientRequest.IdentityKey {
			identityKey[i] = byte(v)
		}
		
		signedPreKey := make([]byte, len(clientRequest.SignedPreKey.PublicKey))
		for i, v := range clientRequest.SignedPreKey.PublicKey {
			signedPreKey[i] = byte(v)
		}
		
		signature := make([]byte, len(clientRequest.SignedPreKey.Signature))
		for i, v := range clientRequest.SignedPreKey.Signature {
			signature[i] = byte(v)
		}
		
		// Store client-provided keys
		if err := getE2EEService().StoreClientProvidedKeys(user.ID, identityKey, signedPreKey, signature, clientRequest.SignedPreKey.KeyID, clientRequest.PreKeys); err != nil {
			log.Printf("Failed to store client-provided E2EE keys for user %s: %v", user.ID, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "Failed to store client-provided E2EE keys",
			})
		}
		
		return c.JSON(fiber.Map{
			"success": true,
			"message": "Client-provided E2EE keys initialized successfully",
		})
	}

	// No client-provided keys, generate on server (legacy approach)
	log.Printf("No client-provided keys, generating E2EE keys on server for user %s", user.ID)
	if err := getE2EEService().InitializeUserKeys(user.ID); err != nil {
		log.Printf("Failed to initialize E2EE keys for user %s: %v", user.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to initialize E2EE keys",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "E2EE keys initialized successfully",
	})
}

// GetPreKeyBundle retrieves the prekey bundle for a user
func GetPreKeyBundle(c fiber.Ctx) error {
	// Get user from context
	_, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get target user ID from params
	targetUserID := c.Params("userId")
	if targetUserID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "User ID is required",
		})
	}

	// Get prekey bundle
	bundle, err := getE2EEService().GetPreKeyBundle(targetUserID)
	if err != nil {
		log.Printf("Failed to get prekey bundle for user %s: %v", targetUserID, err)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "Prekey bundle not found",
		})
	}

	// Convert to response format
	response := PreKeyBundleResponse{
		IdentityKey:    bundle.IdentityKey,
		SignedPreKey:   bundle.SignedPreKey[:],
		Signature:      bundle.Signature,
		PreKeyID:       bundle.PreKeyID,
		SignedPreKeyID: bundle.SignedPreKeyID,
	}

	if bundle.OneTimePreKey != nil {
		response.OneTimePreKey = bundle.OneTimePreKey[:]
	}

	return c.JSON(response)
}

// EncryptMessage encrypts a message for a specific recipient
func EncryptMessage(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Parse request
	var req EncryptMessageRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request format",
		})
	}

	// Validate request
	if req.RecipientID == "" || req.Plaintext == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Recipient ID and plaintext are required",
		})
	}

	// Encrypt message
	ciphertext, err := getE2EEService().EncryptMessage(user.ID, req.RecipientID, req.Plaintext)
	if err != nil {
		log.Printf("Failed to encrypt message from %s to %s: %v", user.ID, req.RecipientID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to encrypt message",
		})
	}

	return c.JSON(EncryptMessageResponse{
		Ciphertext: ciphertext,
		Encrypted:  true,
	})
}

// DecryptMessage decrypts a message from a specific sender
func DecryptMessage(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Parse request
	var req DecryptMessageRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request format",
		})
	}

	// Validate request
	if req.SenderID == "" || req.Ciphertext == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Sender ID and ciphertext are required",
		})
	}

	// Decrypt message
	plaintext, err := getE2EEService().DecryptMessage(user.ID, req.SenderID, req.Ciphertext)
	if err != nil {
		log.Printf("Failed to decrypt message from %s to %s: %v", req.SenderID, user.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to decrypt message",
		})
	}

	return c.JSON(DecryptMessageResponse{
		Plaintext: plaintext,
		Decrypted: true,
	})
}

// CreateGroupSession creates a new group session for E2EE group messaging
func CreateGroupSession(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Parse request
	var req CreateGroupSessionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request format",
		})
	}

	// Validate request
	if req.RoomID == "" || len(req.Participants) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Room ID and participants are required",
		})
	}

	// Create group session
	if err := getE2EEService().CreateGroupSession(req.RoomID, user.ID, req.Participants); err != nil {
		log.Printf("Failed to create group session for room %s: %v", req.RoomID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to create group session",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Group session created successfully",
	})
}

// EncryptGroupMessage encrypts a message for a group
func EncryptGroupMessage(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get room ID from params
	roomID := c.Params("roomId")
	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Room ID is required",
		})
	}

	// Parse request
	var req struct {
		Plaintext string `json:"plaintext"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request format",
		})
	}

	// Validate request
	if req.Plaintext == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Plaintext is required",
		})
	}

	// Encrypt message
	ciphertext, err := getE2EEService().EncryptGroupMessage(roomID, user.ID, req.Plaintext)
	if err != nil {
		log.Printf("Failed to encrypt group message in room %s: %v", roomID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to encrypt group message",
		})
	}

	return c.JSON(EncryptMessageResponse{
		Ciphertext: ciphertext,
		Encrypted:  true,
	})
}

// DecryptGroupMessage decrypts a group message
func DecryptGroupMessage(c fiber.Ctx) error {
	// Get user from context
	_, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get room ID from params
	roomID := c.Params("roomId")
	if roomID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Room ID is required",
		})
	}

	// Parse request
	var req struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request format",
		})
	}

	// Validate request
	if req.Ciphertext == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Ciphertext is required",
		})
	}

	// Decrypt message
	plaintext, err := getE2EEService().DecryptGroupMessage(roomID, req.Ciphertext)
	if err != nil {
		log.Printf("Failed to decrypt group message in room %s: %v", roomID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to decrypt group message",
		})
	}

	return c.JSON(DecryptMessageResponse{
		Plaintext: plaintext,
		Decrypted: true,
	})
}

// RefreshPreKeys generates new one-time prekeys
func RefreshPreKeys(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get count from query params (default to 50)
	count := 50
	if countStr := c.Query("count"); countStr != "" {
		if parsedCount, err := strconv.Atoi(countStr); err == nil && parsedCount > 0 && parsedCount <= 200 {
			count = parsedCount
		}
	}

	// Refresh prekeys
	if err := getE2EEService().RefreshPreKeys(user.ID, count); err != nil {
		log.Printf("Failed to refresh prekeys for user %s: %v", user.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to refresh prekeys",
		})
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Prekeys refreshed successfully",
		"count":   count,
	})
}

// GetE2EEStatus checks if a user has E2EE enabled
func GetE2EEStatus(c fiber.Ctx) error {
	// Get user from context
	user, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Check E2EE status
	enabled, err := getE2EEService().GetUserE2EEStatus(user.ID)
	if err != nil {
		log.Printf("Failed to get E2EE status for user %s: %v", user.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get E2EE status",
		})
	}

	return c.JSON(E2EEStatusResponse{
		Enabled: enabled,
		UserID:  user.ID,
	})
}

// GetUserE2EEStatus checks if a specific user has E2EE enabled
func GetUserE2EEStatus(c fiber.Ctx) error {
	// Get user from context (for authentication)
	_, err := utils.GetUserFromContext(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "Unauthorized",
		})
	}

	// Get target user ID from params
	targetUserID := c.Params("userId")
	if targetUserID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "User ID is required",
		})
	}

	// Check E2EE status
	enabled, err := getE2EEService().GetUserE2EEStatus(targetUserID)
	if err != nil {
		log.Printf("Failed to get E2EE status for user %s: %v", targetUserID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to get E2EE status",
		})
	}

	return c.JSON(E2EEStatusResponse{
		Enabled: enabled,
		UserID:  targetUserID,
	})
}