package portal

import (
	"encoding/json"
	"log"

	"github.com/StrafeChat/equinox/src/database"
	"github.com/livekit/protocol/livekit"
)

type PubData struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

func ProcessWebhookEvent(event *livekit.WebhookEvent) {
	eType := event.GetEvent()

	log.Printf("Webhook received, %v", event)

	switch eType {
	case "participant_joined":
		log.Println("participant joined ", event.GetParticipant().Identity)
		publish(PubData{
			Type: "VOICE_PARTICIPANT_JOIN",
			Data: map[string]interface{}{
				"room_id":        event.GetRoom().Name,
				"participant_id": event.GetParticipant().Identity,
			},
		})
		RegisterJoin(event.GetRoom().Name, event.GetParticipant().Identity)
	case "participant_left":
		log.Println("participant left ", event.GetParticipant().Identity)
		publish(PubData{
			Type: "VOICE_PARTICIPANT_LEAVE",
			Data: map[string]interface{}{
				"room_id":        event.GetRoom().Name,
				"participant_id": event.GetParticipant().Identity,
			},
		})
		RegisterLeave(event.GetRoom().Name, event.GetParticipant().Identity)
	case "room_finished":
		log.Println("Room closed, ", event.GetRoom().Name)
		RoomClosed(event.GetRoom().Name)
	}
}

// publishes a voice update to the respective redis channel
func publish(data PubData) {
	message, err := json.Marshal(data)
	if err != nil {
		log.Printf("[VoicePublish] Failed to marshal update: %v", err)
	}

	if err := database.Rdb.Publish("VOICE_EVENTS", string(message)).Err(); err != nil {
		log.Printf("[VoicePublish] Failed to publish update: %v", err)
	}
}
