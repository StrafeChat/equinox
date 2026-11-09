package messages

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/redistest"
)

// editableMessages is fakeMessages that can also edit what it stored.
type editableMessages struct{ *fakeMessages }

func (f editableMessages) Update(_ context.Context, _, msgID int64, _, plaintext string, embeds *string) (*Message, error) {
	m := f.stored[msgID]
	if m == nil {
		return nil, nil
	}
	if plaintext != "" {
		m.Plaintext = plaintext
	}
	if embeds != nil {
		m.EmbedsJSON = *embeds
	}
	return m, nil
}

// unreadableRooms is fakeRooms whose room read fails.
type unreadableRooms struct{ *fakeRooms }

func (unreadableRooms) GetByID(context.Context, int64) (*rooms.Room, error) {
	return nil, errors.New("scylla: timeout")
}

// wantScope checks one published event: the channel it went out on (the room's own, or a
// user's), and the space and room it is about, in the frame and in the payload alike.
func wantScope(t *testing.T, ev redistest.Event, channelRoom, channelUser, space, room string) {
	t.Helper()
	if ev.SpaceID != channelRoom || ev.UserID != channelUser {
		t.Errorf("%s went out on space %q / user %q, want %q / %q", ev.Type, ev.SpaceID, ev.UserID, channelRoom, channelUser)
	}
	if !ev.Scoped || ev.ScopeSpaceID != space || ev.ScopeRoomID != room {
		t.Errorf("%s scoped %v to space %q room %q, want %q / %q", ev.Type, ev.Scoped, ev.ScopeSpaceID, ev.ScopeRoomID, space, room)
	}
	if got, ok := ev.Data["space_id"]; (space == "" && ok) || (space != "" && got != space) {
		t.Errorf("%s payload space_id %v, want %q", ev.Type, got, space)
	}
	if got := ev.Data["room_id"]; got != room {
		t.Errorf("%s payload room_id %v, want %q", ev.Type, got, room)
	}
}

func eventsOf(published *redistest.Redis, eventType string) []redistest.Event {
	var out []redistest.Event
	for _, ev := range published.Published() {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// A space room's message events carry its space (Discord's guild_id) and its room, a PM's the
// room alone; either way they go out on the room's own channel, never the space's.
func TestMessageEventsNameTheirSpaceAndRoom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pm    bool
		space string
	}{{"space room", false, id.Format(mirrorSpace)}, {"pm", true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := newMirrorWorld()
			if tc.pm {
				w.rooms.room = &rooms.Room{ID: mirrorRoom, Type: rooms.TypePM}
			}
			w.svc.repo = editableMessages{w.repo}
			rdb, published := redistest.New()
			w.svc.redis = rdb
			room := id.Format(mirrorRoom)

			m := &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "hi", CreatedAt: time.Now()}
			if err := w.svc.CreateFederated(ctx, mirrorRoom, []int64{alice, bob, carol}, m); err != nil {
				t.Fatal(err)
			}
			// The edit path has no room loaded: it reads one to learn the space.
			if _, err := w.svc.EditFederated(ctx, mirrorRoom, 901, "", "hi again", nil); err != nil {
				t.Fatal(err)
			}
			if err := w.svc.DeleteFederated(ctx, mirrorRoom, 901); err != nil {
				t.Fatal(err)
			}
			for _, typ := range []string{"MESSAGE_CREATE", "MESSAGE_UPDATE", "MESSAGE_DELETE"} {
				evs := eventsOf(published, typ)
				if len(evs) != 1 {
					t.Fatalf("%s published %d times: %+v", typ, len(evs), published.Published())
				}
				wantScope(t, evs[0], room, "", tc.space, room)
			}
		})
	}
}

// The sender's read receipt goes to their own channel, and still says which space and room.
func TestOwnReadReceiptNamesTheSpace(t *testing.T) {
	w := newMirrorWorld()
	rdb, published := redistest.New()
	w.svc.redis = rdb
	w.fed.create = func(*CreateMessageInput, []Attachment) (*Message, error) {
		return &Message{ID: 901, RoomID: mirrorRoom, SenderID: bob}, nil
	}
	if _, err := w.svc.Create(context.Background(), bob, mirrorRoom, &CreateMessageInput{Plaintext: "hello"}); err != nil {
		t.Fatal(err)
	}
	acks := eventsOf(published, "MESSAGE_ACK")
	if len(acks) != 1 {
		t.Fatalf("events %+v", published.Published())
	}
	wantScope(t, acks[0], "", id.Format(bob), id.Format(mirrorSpace), id.Format(mirrorRoom))
}

// A failed read of the room costs the event its space_id, not the event: it still reaches the
// room's subscribers.
func TestAFailedRoomReadStillPublishes(t *testing.T) {
	w := newMirrorWorld()
	w.repo.stored[901] = &Message{ID: 901, RoomID: mirrorRoom, SenderID: carol, Plaintext: "hi", CreatedAt: time.Now()}
	w.svc.repo = editableMessages{w.repo}
	w.svc.rooms = unreadableRooms{w.rooms}
	rdb, published := redistest.New()
	w.svc.redis = rdb
	if _, err := w.svc.EditFederated(context.Background(), mirrorRoom, 901, "", "edited", nil); err != nil {
		t.Fatal(err)
	}
	evs := eventsOf(published, "MESSAGE_UPDATE")
	if len(evs) != 1 {
		t.Fatalf("events %+v", published.Published())
	}
	wantScope(t, evs[0], id.Format(mirrorRoom), "", "", id.Format(mirrorRoom))
}
