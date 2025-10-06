package portal

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"slices"
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
	ringing      map[string]([]string)
)

func InitPortal() error {
	log.Println("Connecting to Livekit")

	host := os.Getenv("LIVEKIT_HOST")
	key := os.Getenv("LIVEKIT_API_KEY")
	secret := os.Getenv("LIVEKIT_API_SECRET")

	RoomClient = *lksdk.NewRoomServiceClient(host, key, secret)

	rooms = make(map[string]*livekit.Room)
	participants = make(map[string][]string)
	ringing = make(map[string][]string)

	clearRedis()

	return nil
}

func StartRinging(caller, room string) {
	users, ok := ringing[room]
	if !ok {
		users = make([]string, 0)
		ringing[room] = users
	}
	if ok && slices.Contains(users, caller) {
		return
	}

	users = append(users, caller)
	ringing[room] = users

	// TODO: initiate call
	// TODO: add timestamp
	publish(PubData{
		Type: "VOICE_START_RINGING",
		Data: map[string]interface{}{
			"room_id": room,
			"caller":  caller,
		},
	})
	UpdateRedisCall(room)
}
func StopRinging(caller, room string) {
	log.Printf("Stopping call %v, %v", caller, room)
	users, ok := ringing[room]
	if !ok || !slices.Contains(users, caller) {
		return
	}

	idx := slices.Index(users, caller)
	log.Printf("Index %v, %v", idx, users)
	if idx == -1 {
		return
	}

	// remove user from slice
	users[idx] = users[len(users)-1]
	users = users[:len(users)-1]
	ringing[room] = users
	UpdateRedisCall(room)
	publish(PubData{
		Type: "VOICE_STOP_RINGING",
		Data: map[string]interface{}{
			"room_id": room,
			"caller":  caller,
		},
	})
	// TODO:
}

func clearRedis() { // reset data stored in redis
	rdb := database.Rdb
	rMap, err := rdb.Keys("lvcalls:*").Result()
	if err != nil {
		log.Printf("[Portal] Error fetching lvcalls keys: %v", err)
		return
	}

	rdb.Del(rMap...)

	// TODO: possibly reset lvroom keys as well
}

func UpdateRedisRoom(room string) {
	rdb := database.Rdb
	if _, ok := rooms[room]; !ok { // room has been deleted
		_, err := rdb.Del("lvroom:" + room).Result()
		if err != nil {
			log.Printf("[VoiceSync] Error removing room from key storage: %v; error: %v", room, err)
		}
		return
	}

	marshaled, _ := json.Marshal(participants[room])
	err := rdb.Set("lvroom:"+room, string(marshaled), 0)
	if err != nil {
		log.Printf("[VoiceSync] Error updating room %v: %v", room, err)
	}
}
func UpdateRedisCall(room string) {
	rdb := database.Rdb
	us, ok := ringing[room]
	log.Printf("UpdateRedis Call: %v, %v, %v", us, len(us), ok)
	if users, ok := ringing[room]; !ok || (len(users) == 0) { // room has been deleted
		_, err := rdb.Del("lvcalls:" + room).Result()
		if err != nil {
			log.Printf("[VoiceSync] Error removing room from key storage: %v; error: %v", room, err)
		}
		return
	}

	marshaled, _ := json.Marshal(ringing[room])
	err := rdb.Set("lvcalls:"+room, string(marshaled), 0)
	if err != nil {
		log.Printf("[VoiceSync] Error updating room ringing state %v: %v", room, err)
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
	UpdateRedisRoom(room)

	return r
}
func RoomClosed(room string) {
	if _, ok := rooms[room]; !ok {
		delete(rooms, room)

		UpdateRedisRoom(room)
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

	UpdateRedisRoom(room)
}
func RegisterLeave(room, participant string) {
	StopRinging(participant, room)
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

	UpdateRedisRoom(room)
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
