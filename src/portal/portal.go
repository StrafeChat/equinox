package portal

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/livekit/protocol/auth"
	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	RoomClient   lksdk.RoomServiceClient
	rooms        map[string]*livekit.Room
	participants map[string]([]string) // map of participants by room id; updated through webhooks
)

func InitPortal() error {
	log.Println("Connecting to Livekit")

	host := os.Getenv("LIVEKIT_HOST")
	key := os.Getenv("LIVEKIT_API_KEY")
	secret := os.Getenv("LIVEKIT_API_SECRET")

	RoomClient = *lksdk.NewRoomServiceClient(host, key, secret)

	rooms = make(map[string]*livekit.Room)
	participants = make(map[string][]string)

	handler := &EventHandler{}
	log.Print("Starting handler/listener")
	go handler.StartListener()

	return nil
}

type EventHandler struct{}

func (h *EventHandler) StartListener() {
	// subscribe to voice sync requests
	pubsub := database.Rdb.Subscribe("VOICE_EVENTS")
	defer pubsub.Close()

	const numWorkers = 10
	channel := make(chan []byte, 100)

	for i := 0; i < numWorkers; i++ {
		go h.eventWorker(channel)
	}

	ch := pubsub.Channel()
	for msg := range ch {
		log.Printf("Received Redis pub/sub message: Channel=%s, Payload=%s",
			msg.Channel, msg.Payload)

		payload := []byte(strings.TrimSpace(msg.Payload))

		if len(payload) == 0 {
			log.Printf("Received empty payload in pub/sub message")
			continue
		}

		// Send event to worker pool for concurrent processing
		select {
		case channel <- payload:
			// Event queued successfully
		default:
			log.Printf("Event queue full, processing synchronously")
			h.processEvent(payload)
		}
	}
}
func (h *EventHandler) eventWorker(channel <-chan []byte) {
	for payload := range channel {
		h.processEvent(payload)
	}
}
func (h *EventHandler) processEvent(payload []byte) {
	var rawEvent map[string]interface{}
	if err := json.Unmarshal(payload, &rawEvent); err != nil {
		log.Printf("Error unmarshaling event (payload: %s): %v", string(payload), err)
		return
	}

	eventType, ok := rawEvent["type"].(string)
	if !ok || eventType == "" {
		log.Printf("Received event with invalid or empty type: %+v", rawEvent)
		return
	}

	switch eventType {
	case "VOICE_SYNC":
		d, ok := rawEvent["request"].(bool)
		if !ok {
			//log.Printf("Request field not found: %v", d)
			return
		}
		if !d {
			return // sync data not requested
		}

		log.Printf("Voice sync request received")
		// send current voice data
		voiceData := map[string]map[string][]string{
			"rooms": participants,
		}
		log.Printf("%s", participants)
		message, err := json.Marshal(struct {
			Type string                         `json:"type"`
			Data map[string]map[string][]string `json:"data"`
		}{
			Type: "VOICE_SYNC",
			Data: voiceData,
		})
		if err != nil {
			log.Printf("[VoiceSync] Failed to Marshal sync data: %v", err)
			return
		}
		if err := database.Rdb.Publish("VOICE_EVENTS", string(message)).Err(); err != nil {
			log.Printf("[VoiceSync] Failed to Publish sync data: %v", err)
		}
	}
}

func GetJoinToken(room, identity string) string {
	at := auth.NewAccessToken(os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET"))
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
	}
	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(10 * time.Minute)

	token, _ := at.ToJWT()

	if _, ok := rooms[room]; !ok {
		rooms[room] = createRoom(room)
	}
	return token
}

func createRoom(room string) *livekit.Room {
	r, _ := RoomClient.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
		Name:            room,
		EmptyTimeout:    10 * 60, // 10 minutes
		MaxParticipants: 20,      // TODO: edit this?
	})
	return r
}
func RoomClosed(room string) {
	if _, ok := rooms[room]; !ok {
		delete(rooms, room)
	}
}

func RegisterJoin(room, participant string) {
	p, ok := participants[room]
	if !ok {
		p = make([]string, 0)
	}

	if slices.Contains(p, participant) {
		return
	}

	p = append(p, participant)
	participants[room] = p
}
func RegisterLeave(room, participant string) {
	p, ok := participants[room]
	if !ok {
		return
	}

	idx := slices.IndexFunc(p, func(id string) bool {
		return id == participant
	})
	if idx == -1 {
		return
	}
	p = append(p[:idx], p[idx+1:]...)
	participants[room] = p
}
func GetParticipants(room string) []string {
	p, ok := participants[room]
	if !ok {
		p = make([]string, 0)
		participants[room] = p
		return p
	}
	return p
}
