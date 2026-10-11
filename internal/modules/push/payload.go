package push

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/StrafeChat/equinox/internal/logger"
)

const (
	// groupNameParticipants is how many members name a group PM that has no name.
	groupNameParticipants = 3
	// shortNameRunes is what names are cut to when a payload does not fit otherwise.
	shortNameRunes = 64
)

// jobCtx caches what every recipient of one job shares - the room, its space, the author,
// names, the rendered preview - so a message to a hundred people costs one lookup of each,
// not a hundred. Deliveries of one job run in parallel, hence the lock.
type jobCtx struct {
	s   *Service
	job *job

	room   *RoomInfo
	parent *RoomInfo
	space  *SpaceInfo

	mentioned map[int64]struct{}

	mu        sync.Mutex
	users     map[int64]*UserInfo
	rooms     map[int64]*RoomInfo
	roleNames map[int64]string
	members   []int64
	membersOK bool

	previewOnce sync.Once
	preview     string
}

func newJobCtx(s *Service, j *job) *jobCtx {
	jc := &jobCtx{s: s, job: j, users: map[int64]*UserInfo{}, rooms: map[int64]*RoomInfo{}}
	if j.Message != nil {
		jc.mentioned = make(map[int64]struct{}, len(j.Message.Mentioned))
		for _, uid := range j.Message.Mentioned {
			jc.mentioned[uid] = struct{}{}
		}
	}
	return jc
}

// loadRoom loads the room, a thread's channel and the space. False when the room is gone.
func (jc *jobCtx) loadRoom(ctx context.Context) bool {
	dir := jc.s.dir
	room, err := dir.Room(ctx, jc.job.Room)
	if err != nil || room == nil {
		if err != nil {
			logger.Err("push", err, map[string]any{"room_id": jc.job.Room, "step": "load room"})
		}
		return false
	}
	jc.room = room
	if room.Type == roomTypeThread && room.ParentID != 0 {
		jc.parent, _ = dir.Room(ctx, room.ParentID)
	}
	if room.SpaceID != 0 {
		jc.space, _ = dir.Space(ctx, room.SpaceID)
	}
	return true
}

func (jc *jobCtx) mentions(userID int64) bool {
	if jc.job.Message == nil {
		return false
	}
	if jc.job.Message.Everyone {
		return true
	}
	_, ok := jc.mentioned[userID]
	return ok
}

// user looks an account up once per job (nil when unknown).
func (jc *jobCtx) user(ctx context.Context, userID int64) *UserInfo {
	jc.mu.Lock()
	u, ok := jc.users[userID]
	jc.mu.Unlock()
	if ok {
		return u
	}
	u, err := jc.s.dir.User(ctx, userID)
	if err != nil {
		logger.Err("push", err, map[string]any{"user_id": userID, "step": "load user"})
		return nil
	}
	jc.mu.Lock()
	jc.users[userID] = u
	jc.mu.Unlock()
	return u
}

// previewNames for renderPreview, through the job's caches.
func (jc *jobCtx) userName(ctx context.Context, userID int64) (string, bool) {
	if u := jc.user(ctx, userID); u != nil {
		return u.Name(), true
	}
	return "", false
}

func (jc *jobCtx) roleName(ctx context.Context, roleID int64) (string, bool) {
	if jc.room == nil || jc.room.SpaceID == 0 {
		return "", false
	}
	jc.mu.Lock()
	names := jc.roleNames
	jc.mu.Unlock()
	if names == nil {
		var err error
		if names, err = jc.s.dir.RoleNames(ctx, jc.room.SpaceID); err != nil || names == nil {
			names = map[int64]string{}
		}
		jc.mu.Lock()
		jc.roleNames = names
		jc.mu.Unlock()
	}
	n, ok := names[roleID]
	return n, ok && n != ""
}

// roomName resolves a room link for the shared preview: never - a room link's name
// depends on who reads it (see recipientNames), so a text with one is rendered per
// recipient instead.
func (jc *jobCtx) roomName(context.Context, int64) (string, bool) { return "", false }

// lookupRoom loads a linked room once per job.
func (jc *jobCtx) lookupRoom(ctx context.Context, roomID int64) *RoomInfo {
	jc.mu.Lock()
	r, ok := jc.rooms[roomID]
	jc.mu.Unlock()
	if !ok {
		r, _ = jc.s.dir.Room(ctx, roomID)
		jc.mu.Lock()
		jc.rooms[roomID] = r
		jc.mu.Unlock()
	}
	return r
}

// recipientNames resolves names for one recipient's preview. Room links are the reason it
// exists: a link to a private channel must not show its name to someone who cannot see
// that channel, so it is named only when the recipient may view it, and only within the
// message's own space.
type recipientNames struct {
	*jobCtx
	userID int64
}

func (rn recipientNames) roomName(ctx context.Context, roomID int64) (string, bool) {
	r := rn.lookupRoom(ctx, roomID)
	if r == nil || r.Name == "" || rn.room == nil || r.SpaceID == 0 || r.SpaceID != rn.room.SpaceID {
		return "", false
	}
	ok, err := rn.s.dir.CanView(ctx, rn.userID, r)
	if err != nil || !ok {
		return "", false
	}
	return r.Name, true
}

// previewText renders the message's preview: once per job, or per recipient when it
// links a room.
func (jc *jobCtx) previewText(ctx context.Context, recipient int64) string {
	m := jc.job.Message
	if m == nil || m.E2EE || m.Text == "" {
		return ""
	}
	if previewRoomRe.MatchString(m.Text) {
		return renderPreview(ctx, m.Text, recipientNames{jc, recipient})
	}
	jc.previewOnce.Do(func() { jc.preview = renderPreview(ctx, m.Text, jc) })
	return jc.preview
}

// roomName for the payload ("rn"): a space room's or thread's name, a group PM's name or,
// lacking one, its first few other members; nothing for a 1:1 PM, which the device titles
// with the other person's name.
func (jc *jobCtx) conversationName(ctx context.Context, recipient int64) string {
	switch jc.room.Type {
	case roomTypePM:
		return ""
	case roomTypeGroupPM:
		if jc.room.Name != "" {
			return jc.room.Name
		}
		jc.mu.Lock()
		members, ok := jc.members, jc.membersOK
		jc.mu.Unlock()
		if !ok {
			members, _ = jc.s.dir.RoomParticipants(ctx, jc.room.ID)
			jc.mu.Lock()
			jc.members, jc.membersOK = members, true
			jc.mu.Unlock()
		}
		names := make([]string, 0, groupNameParticipants)
		for _, uid := range members {
			if uid == recipient {
				continue
			}
			if u := jc.user(ctx, uid); u != nil && u.Name() != "" {
				names = append(names, u.Name())
				if len(names) == groupNameParticipants {
					break
				}
			}
		}
		return strings.Join(names, ", ")
	default:
		return jc.room.Name
	}
}

// author is the "a" object for a sender or caller.
func (jc *jobCtx) author(ctx context.Context, userID int64) *Author {
	a := &Author{ID: formatID(userID), Name: "Someone"}
	if u := jc.user(ctx, userID); u != nil {
		if n := u.Name(); n != "" {
			a.Name = n
		}
		if strings.HasPrefix(u.Avatar, "https://") || strings.HasPrefix(u.Avatar, "http://") {
			a.Avatar = u.Avatar
		}
	}
	return a
}

// basePayload is the routing part every room-scoped payload carries; read adds the
// message it was read up to.
func (jc *jobCtx) basePayload(recipient int64, t string, withReadMark bool) ([]byte, error) {
	j := jc.job
	p := Payload{V: PayloadVersion, T: t, UID: formatID(recipient), TS: j.TS, RoomID: formatID(j.Room), RoomType: j.RoomType}
	if t != TypeCallEnd && j.Space != 0 {
		p.SpaceID = formatID(j.Space)
	}
	if withReadMark && j.ReadTo != 0 {
		p.MessageID = formatID(j.ReadTo)
	}
	return marshalPayload(&p)
}

// messagePayload builds a "msg" payload for one recipient. The body preview is included
// only for a room that is not end-to-end encrypted and a device that wants previews.
func (jc *jobCtx) messagePayload(ctx context.Context, recipient int64, preview, mentioned bool) ([]byte, error) {
	j, m := jc.job, jc.job.Message
	p := Payload{
		V: PayloadVersion, T: TypeMessage, UID: formatID(recipient), TS: j.TS,
		RoomID: formatID(jc.room.ID), RoomType: jc.room.Type, MessageID: formatID(m.ID),
		Author:    jc.author(ctx, m.Sender),
		RoomName:  jc.conversationName(ctx, recipient),
		Encrypted: m.E2EE,
		Attach:    m.Attachments,
		Mentioned: mentioned,
	}
	if jc.room.SpaceID != 0 {
		p.SpaceID = formatID(jc.room.SpaceID)
		if jc.space != nil {
			p.SpaceName = jc.space.Name
		}
	}
	if jc.parent != nil {
		p.ParentName = jc.parent.Name
	}
	if preview && !m.E2EE {
		p.Body = jc.previewText(ctx, recipient)
	}
	return marshalPayload(&p)
}

// callPayload builds a "call" payload: who is calling, and the group's name.
func (jc *jobCtx) callPayload(ctx context.Context, recipient int64) ([]byte, error) {
	j := jc.job
	p := Payload{
		V: PayloadVersion, T: TypeCall, UID: formatID(recipient), TS: j.TS,
		RoomID: formatID(jc.room.ID), RoomType: jc.room.Type,
		Author:   jc.author(ctx, j.Caller),
		RoomName: jc.conversationName(ctx, recipient),
	}
	return marshalPayload(&p)
}

// marshalPayload encodes a payload, shedding optional detail until it fits the largest
// padding bucket: first the preview, then the avatar, then long names are shortened.
func marshalPayload(p *Payload) ([]byte, error) {
	for step := 0; ; step++ {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxPayloadBytes {
			return b, nil
		}
		switch step {
		case 0:
			p.Body = ""
		case 1:
			if p.Author != nil {
				p.Author.Avatar = ""
			}
		case 2:
			p.RoomName = truncateRunes(p.RoomName, shortNameRunes)
			p.ParentName = truncateRunes(p.ParentName, shortNameRunes)
			p.SpaceName = truncateRunes(p.SpaceName, shortNameRunes)
			if p.Author != nil {
				p.Author.Name = truncateRunes(p.Author.Name, shortNameRunes)
			}
		default:
			return nil, errPayloadTooLarge
		}
	}
}
