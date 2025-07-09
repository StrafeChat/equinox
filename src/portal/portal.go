package portal

import (
	"context"
	"os"
	"slices"
	"time"

	"github.com/livekit/protocol/auth"
	livekit "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	RoomClient lksdk.RoomServiceClient
	rooms      []string
)

func InitPortal() error {

	host := os.Getenv("LIVEKIT_HOST")
	key := os.Getenv("LIVEKIT_API_KEY")
	secret := os.Getenv("LIVEKIT_API_SECRET")

	RoomClient = *lksdk.NewRoomServiceClient(host, key, secret)

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

	if !slices.Contains(rooms, room) {
		createRoom(room)
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
