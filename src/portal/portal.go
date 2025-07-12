package portal

import (
	"context"
	"log"
	"os"
	"slices"
	"time"

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

	return nil
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
