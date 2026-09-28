package voice

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
)

// How long a join token stays valid. LiveKit checks it when the client connects and
// on every full reconnect, so it has to outlast a normal session; the client asks
// for a fresh one whenever a reconnect is refused.
const tokenTTL = 12 * time.Hour

// LiveKit wraps the server SDK: tokens, room service calls and webhook verification.
type LiveKit struct {
	cfg   config.VoiceConfig
	rooms *lksdk.RoomServiceClient
	keys  auth.KeyProvider
}

func NewLiveKit(cfg config.VoiceConfig) *LiveKit {
	return &LiveKit{
		cfg:   cfg,
		rooms: lksdk.NewRoomServiceClient(cfg.InternalURL, cfg.APIKey, cfg.APISecret),
		keys:  auth.NewSimpleKeyProvider(cfg.APIKey, cfg.APISecret),
	}
}

// PublicURL is what browsers connect to.
func (l *LiveKit) PublicURL() string { return l.cfg.PublicURL }

// RoomName maps a Strafe room to its LiveKit room. One LiveKit room per Strafe room;
// the prefix keeps the namespace ours should the server ever be shared.
func RoomName(roomID int64) string { return "strafe_" + id.Format(roomID) }

// Token issues a join token for identity in roomName with the given publish/subscribe
// rights. sources lists what the participant may publish; empty means nothing.
func (l *LiveKit) Token(identity, displayName, roomName string, canSubscribe bool, sources []livekit.TrackSource) (string, error) {
	grant := &auth.VideoGrant{RoomJoin: true, Room: roomName}
	grant.SetCanSubscribe(canSubscribe)
	grant.SetCanPublishData(true)
	grant.SetCanPublish(len(sources) > 0)
	grant.SetCanPublishSources(sources)
	return auth.NewAccessToken(l.cfg.APIKey, l.cfg.APISecret).
		SetIdentity(identity).
		SetName(displayName).
		SetValidFor(tokenTTL).
		SetVideoGrant(grant).
		ToJWT()
}

// EnsureRoom creates the LiveKit room if it doesn't exist (CreateRoom returns the
// existing room otherwise). The empty timeout keeps a room alive briefly between the
// last leave and the next join so quick rejoins don't race its teardown.
func (l *LiveKit) EnsureRoom(ctx context.Context, name string) error {
	_, err := l.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:             name,
		EmptyTimeout:     30,
		DepartureTimeout: 10,
	})
	return err
}

// RemoveParticipant kicks identity from the room. Not-found is not an error: the
// participant may already be gone.
func (l *LiveKit) RemoveParticipant(ctx context.Context, roomName, identity string) error {
	_, err := l.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: roomName, Identity: identity})
	if isNotFound(err) {
		return nil
	}
	return err
}

// SetPermission replaces a connected participant's publish/subscribe rights. LiveKit
// enforces it immediately: a revoked microphone stops being forwarded and cannot be
// re-published, a revoked subscription stops every incoming track.
func (l *LiveKit) SetPermission(ctx context.Context, roomName, identity string, canSubscribe bool, sources []livekit.TrackSource) error {
	_, err := l.rooms.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:     roomName,
		Identity: identity,
		Permission: &livekit.ParticipantPermission{
			CanSubscribe:      canSubscribe,
			CanPublish:        len(sources) > 0,
			CanPublishData:    true,
			CanPublishSources: sources,
		},
	})
	if isNotFound(err) {
		return nil
	}
	return err
}

// MuteSource server-mutes every published track of the given source (the microphone,
// for a server mute). No-op when the participant has no such track.
func (l *LiveKit) MuteSource(ctx context.Context, roomName, identity string, source livekit.TrackSource) error {
	p, err := l.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: roomName, Identity: identity})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	for _, t := range p.Tracks {
		if t.Source != source || t.Muted {
			continue
		}
		if _, err := l.rooms.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
			Room: roomName, Identity: identity, TrackSid: t.Sid, Muted: true,
		}); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// ListIdentities returns the identities currently connected to the room. A room
// LiveKit doesn't know is simply empty.
func (l *LiveKit) ListIdentities(ctx context.Context, roomName string) (map[string]struct{}, error) {
	res, err := l.rooms.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: roomName})
	if err != nil {
		if isNotFound(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	out := make(map[string]struct{}, len(res.Participants))
	for _, p := range res.Participants {
		out[p.Identity] = struct{}{}
	}
	return out, nil
}

// ReceiveWebhook verifies a webhook request was signed with our key pair and parses it.
func (l *LiveKit) ReceiveWebhook(r *http.Request) (*livekit.WebhookEvent, error) {
	return webhook.ReceiveWebhookEvent(r, l.keys)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var te twirp.Error
	if errors.As(err, &te) {
		return te.Code() == twirp.NotFound
	}
	return false
}

// Track sources a participant may publish, derived from their permissions and server
// mute. Video covers the camera and screen sharing (with its audio).
func sourcesFor(speak, video, serverMuted bool) []livekit.TrackSource {
	var out []livekit.TrackSource
	if speak && !serverMuted {
		out = append(out, livekit.TrackSource_MICROPHONE)
	}
	if video {
		out = append(out, livekit.TrackSource_CAMERA, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO)
	}
	return out
}
