package voice

import (
	"context"

	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// The rest of Federator, so fakeFed keeps satisfying it. These answer nothing; recordingFed in
// fixture_test.go overrides the ones a test watches.

func (f *fakeFed) SpaceFederation(context.Context, int64) *spaces.SpaceFederation { return nil }

func (f *fakeFed) LeaveRemoteVoiceSession(context.Context, string, *rooms.RoomWithParticipants, *auth.User, string) {
}

func (f *fakeFed) ModerateRemoteVoice(context.Context, string, int64, *auth.User, int64, ModerateInput) error {
	return ErrOriginUnavailable
}

func (f *fakeFed) DisconnectRemoteVoice(context.Context, string, int64, *auth.User, int64) error {
	return ErrOriginUnavailable
}

func (f *fakeFed) RemoteVoiceStates(context.Context, string, []*rooms.RoomWithParticipants) ([]RoomSync, error) {
	return nil, ErrOriginUnavailable
}

func (f *fakeFed) AfterVoiceStateMoved(context.Context, *rooms.RoomWithParticipants, *State, *rooms.RoomWithParticipants) {
}
