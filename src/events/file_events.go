package events

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/StrafeChat/equinox/src/types"
	"github.com/google/uuid"
)

// FileMetadataRequest represents a file metadata request to Redis
type FileMetadataRequest struct {
	Type      string `json:"type"`
	FileID    string `json:"file_id"`
	RequestID string `json:"request_id"`
	CreatedAt int64  `json:"created_at"`
}

// FileMetadataResponse represents a file metadata response from Redis
type FileMetadataResponse struct {
	Type      string              `json:"type"`
	RequestID string              `json:"request_id"`
	FileID    string              `json:"file_id"`
	Success   bool                `json:"success"`
	Data      *types.FileMetadata `json:"data,omitempty"`
	Error     string              `json:"error,omitempty"`
	CreatedAt int64               `json:"created_at"`
}

// PendingRequest represents a pending file metadata request
type PendingRequest struct {
	RequestID string
	FileID    string
	Response  chan *types.FileMetadata
	Error     chan error
	CreatedAt time.Time
}

var (
	pendingRequests = make(map[string]*PendingRequest)
	requestsMutex   sync.RWMutex
	requestTimeout  = 10 * time.Second
)

// StartFileEventListener starts listening for file events from Redis
func StartFileEventListener() {
	log.Printf("Starting Redis file event listener")

	// Subscribe to FILE_EVENTS channel
	pubsub := database.Rdb.Subscribe("FILE_EVENTS")
	defer pubsub.Close()

	ch := pubsub.Channel()
	log.Printf("Successfully subscribed to FILE_EVENTS channel")

	// Start cleanup goroutine for expired requests
	go cleanupExpiredRequests()

	for msg := range ch {
		log.Printf("[StartFileEventListener] Received file event: %s", msg.Payload)

		payload := []byte(strings.TrimSpace(msg.Payload))

		if len(payload) == 0 {
			log.Printf("[StartFileEventListener] Received empty payload in file event")
			continue
		}

		var response FileMetadataResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			log.Printf("[StartFileEventListener] Error unmarshaling file event (payload: %s): %v", string(payload), err)
			continue
		}

		if response.Type == "" {
			log.Printf("[StartFileEventListener] Received file event with empty type: %+v", response)
			continue
		}

		log.Printf("[StartFileEventListener] Processing file event: Type=%s, RequestID=%s", response.Type, response.RequestID)

		switch response.Type {
		case "FILE_METADATA_RESPONSE":
			log.Printf("[StartFileEventListener] Handling FILE_METADATA_RESPONSE event")
			go handleFileMetadataResponse(response)
		default:
			log.Printf("[StartFileEventListener] Unknown file event type: %s", response.Type)
		}
	}
}

// RequestFileMetadata requests file metadata from Nebula via Redis pub/sub
func RequestFileMetadata(fileID string) (*types.FileMetadata, error) {
	log.Printf("[RequestFileMetadata] Requesting metadata for file: %s", fileID)

	// Generate unique request ID
	requestID := uuid.New().String()

	// Create pending request
	pendingReq := &PendingRequest{
		RequestID: requestID,
		FileID:    fileID,
		Response:  make(chan *types.FileMetadata, 1),
		Error:     make(chan error, 1),
		CreatedAt: time.Now(),
	}

	// Store pending request
	requestsMutex.Lock()
	pendingRequests[requestID] = pendingReq
	requestsMutex.Unlock()

	// Create request payload
	request := FileMetadataRequest{
		Type:      "FILE_METADATA_REQUEST",
		FileID:    fileID,
		RequestID: requestID,
		CreatedAt: time.Now().Unix(),
	}

	// Marshal request to JSON
	requestBytes, err := json.Marshal(request)
	if err != nil {
		log.Printf("[RequestFileMetadata] Failed to marshal request: %v", err)
		// Clean up pending request
		requestsMutex.Lock()
		delete(pendingRequests, requestID)
		requestsMutex.Unlock()
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	// Publish request to Redis
	log.Printf("[RequestFileMetadata] Publishing request to Redis: %s", string(requestBytes))
	if err := database.Rdb.Publish("FILE_EVENTS", string(requestBytes)).Err(); err != nil {
		log.Printf("[RequestFileMetadata] Failed to publish request: %v", err)
		// Clean up pending request
		requestsMutex.Lock()
		delete(pendingRequests, requestID)
		requestsMutex.Unlock()
		return nil, fmt.Errorf("failed to publish request: %v", err)
	}

	// Wait for response with timeout
	select {
	case metadata := <-pendingReq.Response:
		log.Printf("[RequestFileMetadata] Received successful response for file: %s", fileID)
		return metadata, nil
	case err := <-pendingReq.Error:
		log.Printf("[RequestFileMetadata] Received error response for file: %s, error: %v", fileID, err)
		return nil, err
	case <-time.After(requestTimeout):
		log.Printf("[RequestFileMetadata] Request timeout for file: %s", fileID)
		// Clean up pending request
		requestsMutex.Lock()
		delete(pendingRequests, requestID)
		requestsMutex.Unlock()
		return nil, fmt.Errorf("request timeout for file: %s", fileID)
	}
}

// handleFileMetadataResponse handles file metadata response events
func handleFileMetadataResponse(response FileMetadataResponse) {
	log.Printf("[handleFileMetadataResponse] Handling response for RequestID: %s", response.RequestID)

	// Find pending request
	requestsMutex.RLock()
	pendingReq, exists := pendingRequests[response.RequestID]
	requestsMutex.RUnlock()

	if !exists {
		log.Printf("[handleFileMetadataResponse] No pending request found for RequestID: %s", response.RequestID)
		return
	}

	// Remove from pending requests
	requestsMutex.Lock()
	delete(pendingRequests, response.RequestID)
	requestsMutex.Unlock()

	if response.Success {
		log.Printf("[handleFileMetadataResponse] Sending successful response for RequestID: %s", response.RequestID)
		select {
		case pendingReq.Response <- response.Data:
			// Response sent successfully
		default:
			// Channel might be closed or full
			log.Printf("[handleFileMetadataResponse] Failed to send response to channel for RequestID: %s", response.RequestID)
		}
	} else {
		log.Printf("[handleFileMetadataResponse] Sending error response for RequestID: %s, error: %s", response.RequestID, response.Error)
		select {
		case pendingReq.Error <- fmt.Errorf(response.Error):
			// Error sent successfully
		default:
			// Channel might be closed or full
			log.Printf("[handleFileMetadataResponse] Failed to send error to channel for RequestID: %s", response.RequestID)
		}
	}
}

// cleanupExpiredRequests periodically cleans up expired pending requests
func cleanupExpiredRequests() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		requestsMutex.Lock()
		for requestID, pendingReq := range pendingRequests {
			if now.Sub(pendingReq.CreatedAt) > requestTimeout {
				log.Printf("[cleanupExpiredRequests] Cleaning up expired request: %s", requestID)
				// Send timeout error
				select {
				case pendingReq.Error <- fmt.Errorf("request timeout"):
				default:
					// Channel might be closed or full
				}
				delete(pendingRequests, requestID)
			}
		}
		requestsMutex.Unlock()
	}
}
