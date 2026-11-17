package rooms

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/config"
	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// A PM keeps a deleted participant as a placeholder: without it the other side would see a
// one-person PM and take it for their Notes room.

type deletionUsers struct {
	auth.UserRepository
	rows map[int64]*auth.User
	err  error
}

func (f deletionUsers) GetByIDs(_ context.Context, ids []int64) ([]*auth.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*auth.User, len(ids))
	for i, uid := range ids {
		out[i] = f.rows[uid]
	}
	return out, nil
}

func TestEnrichEmitsDeletedPlaceholder(t *testing.T) {
	at := time.Now().UTC()
	rid := int64(99)
	cfg := &config.Config{}
	cfg.Federation.Domain = "a.test"
	s := &Service{cfg: cfg, user: deletionUsers{rows: map[int64]*auth.User{
		1: {ID: 1, Username: "bob"},
		3: {ID: 3, Username: "", HomeDomain: "b.test", RemoteID: &rid, DeletedAt: &at},
	}}}

	rwp := &RoomWithParticipants{Room: Room{ID: 50, Type: TypePM}, ParticipantIDs: []int64{1, 2}}
	s.enrich(context.Background(), rwp)
	if len(rwp.Participants) != 2 {
		t.Fatalf("participants %+v", rwp.Participants)
	}
	if p := rwp.Participants[0]; p.Deleted || p.Username != "bob" {
		t.Errorf("live participant %+v", p)
	}
	gone := rwp.Participants[1]
	if !gone.Deleted || gone.ID != id.Format(2) || gone.Username != "" || gone.HomeDomain != "a.test" || gone.OriginID != "2" {
		t.Errorf("deleted local participant %+v", gone)
	}

	// A tombstoned shadow: deleted, blank, still its federated identity.
	tomb := &RoomWithParticipants{Room: Room{ID: 51, Type: TypePM}, ParticipantIDs: []int64{1, 3}}
	s.enrich(context.Background(), tomb)
	if p := tomb.Participants[1]; !p.Deleted || p.Username != "" || p.HomeDomain != "b.test" || p.OriginID != "99" {
		t.Errorf("tombstone participant %+v", p)
	}

	// A failed read is not a deletion: no placeholders.
	s.user = deletionUsers{err: errors.New("down")}
	failed := &RoomWithParticipants{Room: Room{ID: 52, Type: TypePM}, ParticipantIDs: []int64{1, 2}}
	s.enrich(context.Background(), failed)
	if len(failed.Participants) != 0 {
		t.Errorf("a failed read produced %+v", failed.Participants)
	}
}
