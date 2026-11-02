package spaces

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/stargate"
)

// ReadState is a user's read state across their rooms: their rooms_by_user rows and the raw
// mention totals, each one partition read, shared by every space a room list is built for.
type ReadState struct {
	Rows     map[int64]*rooms.RoomRow
	Mentions map[int64]int
}

// LoadReadState reads a user's read state. A failed read leaves that half empty: the rooms
// still go out, without their read markers, as READY has always done.
func (s *Service) LoadReadState(ctx context.Context, userID int64) ReadState {
	rows, _ := s.UserRoomRows(ctx, userID)
	mentions, _ := s.roomRepo.GetMentionCounts(ctx, userID)
	return ReadState{Rows: rows, Mentions: mentions}
}

// ViewerSpaceRooms is a user's rooms of one space in the form READY's space_rooms[space_id]
// entries have - SPACE_CREATE's rooms are built here too, so the two never differ: the rooms
// they may view (VisibleSpaceRooms, active threads included), each with its permission
// overrides and global identity, a thread with its counts and whether the user joined, and
// the user's read markers and mention count. The rooms and the snapshot come back as well,
// for callers that need more of them.
func (s *Service) ViewerSpaceRooms(ctx context.Context, userID, spaceID int64, rs ReadState) ([]map[string]interface{}, []*rooms.Room, *Snapshot, error) {
	list, snap, err := s.VisibleSpaceRooms(ctx, userID, spaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	roomIDs := make([]int64, 0, len(list))
	var threadIDs []int64
	for _, r := range list {
		roomIDs = append(roomIDs, r.ID)
		if r.Type == rooms.TypeThread {
			threadIDs = append(threadIDs, r.ID)
		}
	}
	feds := s.RoomFederations(ctx, roomIDs)
	// Active threads ride in the list (type 6) with their counts and whether this user
	// joined - Discord's thread sync on READY.
	threadStates, _ := s.ThreadStates(ctx, userID, threadIDs)
	out := make([]map[string]interface{}, 0, len(list))
	for _, r := range list {
		m := AttachOverrides(RoomMap(r), snap.RoomOverridesFor(r.ID))
		AttachFederation(m, feds[r.ID])
		if r.Type == rooms.TypeThread {
			AttachThreadState(m, threadStates[r.ID])
		}
		ur := rs.Rows[r.ID]
		if ur != nil {
			if ur.LastReadMessageID != nil {
				m["last_read_message_id"] = id.Format(*ur.LastReadMessageID)
			}
			if ur.LastMessageID != nil {
				m["last_message_id"] = id.Format(*ur.LastMessageID)
			}
		}
		if mc := ur.DisplayMentionCount(rs.Mentions[r.ID]); mc > 0 {
			m["mention_count"] = mc
		}
		out = append(out, m)
	}
	return out, list, snap, nil
}

// publishSpaceCreate sends SPACE_CREATE to someone who now belongs to the space: the space
// with its roles and, like Discord's GUILD_CREATE with its channels, the rooms they may view
// in READY's form, so a client - or a bot's connection - has the whole space without another
// request. The event subscribes their sockets to the space and those rooms as the gateway
// delivers it, so whatever the space publishes next reaches them live; callers send it right
// after the membership write and before anything else about the join. An account of another
// instance has no socket here and is sent nothing. Without the room list (a failed read) the
// space goes out without rooms, which a client then loads itself.
func (s *Service) publishSpaceCreate(ctx context.Context, sp *Space, userID int64) {
	if s.redis == nil {
		return
	}
	if u, _ := s.userRepo.GetByID(ctx, userID); u != nil && u.IsRemote() {
		return
	}
	s.attachFederation(ctx, sp)
	space := spaceToEventPayload(sp)
	if roles, err := s.SpaceRoles(ctx, sp.ID); err == nil {
		space["roles"] = RoleMaps(roles)
	}
	if sp.EveryoneRoleID != 0 {
		space["everyone_role_id"] = id.Format(sp.EveryoneRoleID)
	}
	opens := []int64{sp.ID}
	if roomMaps, list, _, err := s.ViewerSpaceRooms(ctx, userID, sp.ID, s.LoadReadState(ctx, userID)); err == nil {
		space["rooms"] = roomMaps
		for _, r := range list {
			opens = append(opens, r.ID)
		}
	}
	sc := stargate.InSpace(sp.ID).Subscribing(opens...)
	stargate.PublishToUserIn(ctx, s.redis, userID, sc, "SPACE_CREATE", space, s.stargateRegion())
}
