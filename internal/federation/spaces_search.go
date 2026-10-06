package federation

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// Search in a space hosted elsewhere runs on the origin (messages.Federator.SearchRemote /
// SearchSpaceRemote): a mirror only holds what was relayed since one of its members
// joined, so searching its own copy would quietly miss everything older. The origin runs
// its normal search as the asking member (so channel permissions and E2EE skipping apply
// as for a local member) and answers with message events the mirror converts and stores
// like a history page.

// RemoteSpaceOrigin names the instance hosting a space when it is not this one.
func (s *Service) RemoteSpaceOrigin(ctx context.Context, spaceID int64) string {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil || m == nil || s.IsLocalServer(m.OriginDomain) {
		return ""
	}
	return m.OriginDomain
}

func (s *Service) spaceRefOf(ctx context.Context, spaceID int64) (SpaceRef, error) {
	m, err := s.repo.GetSpaceMapping(ctx, spaceID)
	if err != nil {
		return SpaceRef{}, err
	}
	if m == nil {
		return SpaceRef{}, rooms.ErrRoomNotFound
	}
	return SpaceRef{OriginDomain: m.OriginDomain, OriginSpaceID: id.Format(m.OriginSpaceID)}, nil
}

// searchFilters fills the FID-valued filters. A filter naming a user this instance does
// not know matches nothing, which is what the asker would get locally too; ok is false then.
func (s *Service) searchFilters(ctx context.Context, opts *messages.SearchOptions, req *SpaceSearchQuery) (ok bool) {
	fid := func(uid *int64) (string, bool) {
		if uid == nil {
			return "", true
		}
		u, err := s.users.GetByID(ctx, *uid)
		if err != nil || u == nil {
			return "", false
		}
		return s.FIDOf(u), true
	}
	var okFrom, okMentions bool
	if req.From, okFrom = fid(opts.FromUserID); !okFrom {
		return false
	}
	if req.Mentions, okMentions = fid(opts.MentionsUserID); !okMentions {
		return false
	}
	req.Query = opts.Query
	req.Has = opts.Has
	req.Limit = opts.Limit
	if opts.BeforeID != nil {
		req.Before = id.Format(*opts.BeforeID)
	}
	return true
}

func (s *Service) SearchRemote(ctx context.Context, origin string, room *rooms.Room, userID int64, opts *messages.SearchOptions) (*messages.SearchResult, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil || room.SpaceID == nil {
		return nil, messages.ErrNotParticipant
	}
	_, ref, err := s.mirrorScope(ctx, room)
	if err != nil {
		return nil, err
	}
	space, err := s.spaceRefOf(ctx, *room.SpaceID)
	if err != nil {
		return nil, err
	}
	req := SpaceSearchQuery{Space: space, Room: &ref, User: s.FIDOf(user)}
	if !s.searchFilters(ctx, opts, &req) {
		return &messages.SearchResult{Messages: []messages.Message{}, Searchable: true}, nil
	}
	reply, msgs, err := s.searchRemote(ctx, origin, req)
	if err != nil {
		return nil, err
	}
	res := &messages.SearchResult{Messages: msgs, Searchable: true}
	if reply.NextBefore != "" {
		if v, perr := id.Parse(reply.NextBefore); perr == nil {
			res.NextBeforeID = &v
		}
	}
	return res, nil
}

func (s *Service) SearchSpaceRemote(ctx context.Context, origin string, spaceID, userID int64, opts *messages.SpaceSearchOptions) (*messages.SpaceSearchResult, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil || user == nil {
		return nil, messages.ErrNotParticipant
	}
	space, err := s.spaceRefOf(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	req := SpaceSearchQuery{Space: space, User: s.FIDOf(user)}
	if opts.RoomID != nil {
		room, err := s.roomRepo.GetByID(ctx, *opts.RoomID)
		if err != nil {
			return nil, err
		}
		if room == nil {
			return nil, rooms.ErrRoomNotFound
		}
		_, ref, err := s.mirrorScope(ctx, room)
		if err != nil {
			return nil, err
		}
		req.Room = &ref
	}
	if !s.searchFilters(ctx, &opts.SearchOptions, &req) {
		return &messages.SpaceSearchResult{Messages: []messages.Message{}}, nil
	}
	reply, msgs, err := s.searchRemote(ctx, origin, req)
	if err != nil {
		return nil, err
	}
	res := &messages.SpaceSearchResult{Messages: msgs, RoomsSearched: reply.RoomsSearched, EncryptedRooms: reply.EncryptedRooms}
	if reply.NextBefore != "" {
		if v, perr := id.Parse(reply.NextBefore); perr == nil {
			res.NextBeforeID = &v
		}
	}
	return res, nil
}

// searchRemote runs the query on the origin and converts the hits into local messages
// (stored like history, so opening a hit works offline too). Hits in a channel this
// mirror has no row for are skipped.
func (s *Service) searchRemote(ctx context.Context, origin string, req SpaceSearchQuery) (*SpaceSearchReply, []messages.Message, error) {
	var reply SpaceSearchReply
	if _, err := s.client.Do(ctx, origin, http.MethodPost, "/spaces/messages/search", req, &reply); err != nil {
		return nil, nil, messageOriginError(err, origin, "/spaces/messages/search")
	}
	scopes := map[string]*roomScope{}
	out := make([]messages.Message, 0, len(reply.Messages))
	for _, ev := range reply.Messages {
		sc, ok := scopes[ev.Room.OriginRoomID]
		if !ok {
			var err error
			if sc, err = s.scopeForRef(ctx, origin, ev.Room); err != nil {
				logger.Err("federation", err, map[string]any{"peer": origin, "room": ev.Room.OriginRoomID, "stage": "search_scope"})
				continue
			}
			scopes[ev.Room.OriginRoomID] = sc
		}
		m, err := s.messageFromEvent(ctx, origin, sc, ev)
		if err != nil {
			logger.Err("federation", err, map[string]any{"peer": origin, "message": ev.Message.OriginMessageID})
			continue
		}
		s.storeHistory(ctx, sc, m)
		out = append(out, *m)
	}
	return &reply, out, nil
}

// scopeForRef is the mirror's scope for one of the origin's channels, by its wire ref.
func (s *Service) scopeForRef(ctx context.Context, origin string, ref RoomRef) (*roomScope, error) {
	originRoomID, err := id.Parse(ref.OriginRoomID)
	if err != nil {
		return nil, ErrInvalidFID
	}
	localID, err := s.localRoomID(ctx, origin, originRoomID, false)
	if err != nil {
		return nil, err
	}
	room, err := s.roomRepo.GetByID(ctx, localID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, rooms.ErrRoomNotFound
	}
	sc, _, err := s.mirrorScope(ctx, room)
	return sc, err
}

// SpaceMessagesSearch POST /spaces/messages/search - the origin's side: search a hosted
// space (or one channel of it) as the asking member.
func (h *Handler) SpaceMessagesSearch(c fiber.Ctx) error {
	var body SpaceSearchQuery
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
	}
	sp, err := h.hostedSpace(c, body.Space)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	actor, err := h.spaceActor(c, body.User)
	if err != nil {
		return spaceFail(c, err, nil)
	}
	ctx := c.Context()
	opts := &messages.SpaceSearchOptions{SearchOptions: messages.SearchOptions{Query: body.Query, Has: body.Has, Limit: body.Limit}}
	empty := func() error {
		return c.JSON(SpaceSearchReply{Messages: []MessageEvent{}})
	}
	if body.From != "" {
		uid, err := h.svc.ResolveLocalID(ctx, body.From)
		if err != nil {
			return empty() // a person nobody here knows has written nothing here
		}
		opts.FromUserID = &uid
	}
	if body.Mentions != "" {
		uid, err := h.svc.ResolveLocalID(ctx, body.Mentions)
		if err != nil {
			return empty()
		}
		opts.MentionsUserID = &uid
	}
	if body.Before != "" {
		if v, perr := id.Parse(body.Before); perr == nil {
			opts.BeforeID = &v
		}
	}
	if body.Room != nil {
		sc, err := h.roomFor(c, *body.Room)
		if err != nil {
			return spaceFail(c, err, nil)
		}
		if sc.spaceID != sp.ID || !sc.hostedHere {
			return spaceFail(c, errNotSpaceOrigin, nil)
		}
		rid := sc.room.ID
		opts.RoomID = &rid
	}
	res, err := h.svc.msgSvc.SearchSpaceMessages(ctx, actor.ID, sp.ID, opts)
	if err != nil {
		return spaceFail(c, err, map[string]any{"space_id": sp.ID})
	}
	reply := SpaceSearchReply{Messages: make([]MessageEvent, 0, len(res.Messages)), RoomsSearched: res.RoomsSearched, EncryptedRooms: res.EncryptedRooms}
	for i := range res.Messages {
		ref, err := h.svc.roomRef(ctx, res.Messages[i].RoomID)
		if err != nil {
			continue
		}
		reply.Messages = append(reply.Messages, h.svc.messageEvent(ctx, ref, &res.Messages[i], nil))
	}
	if res.NextBeforeID != nil {
		reply.NextBefore = id.Format(*res.NextBeforeID)
	}
	return c.JSON(reply)
}
