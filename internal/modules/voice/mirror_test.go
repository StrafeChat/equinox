package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// ---- who hears a state ---------------------------------------------------------------------

func TestOpenRoomStateGoesToTheSpace(t *testing.T) {
	w := newWorld(t)
	w.svc.publishState(context.Background(), w.room(wLounge), w.state(wLounge, wCarol, "s1"), false)
	got := w.voiceEvents(wLounge)
	if len(got) != 1 || got[0].SpaceID != id.Format(wSpace) {
		t.Fatalf("events %+v, want one on the space channel", got)
	}
}

// READY and the states endpoint keep a private room's occupants from a member who cannot view
// it; a live event must not hand them over anyway. It goes on the room's own channel, which the
// gateway lets only viewers subscribe to, so nobody else's socket receives it and no member list
// is read on the way.
func TestPrivateRoomStateGoesToTheRoomsOwnChannel(t *testing.T) {
	w := newWorld(t)
	w.svc.publishState(context.Background(), w.room(wSecret), w.state(wSecret, wAlice, "s1"), false)
	got := w.voiceEvents(wSecret)
	if len(got) != 2 || got[0].SpaceID != id.Format(wSecret) || got[1].UserID != id.Format(wAlice) {
		t.Fatalf("events %+v, want the room's channel and the state's own user", got)
	}
}

// Someone who lost View is no viewer, and still has to hear they left.
func TestPrivateRoomStateAlwaysReachesItsOwnUser(t *testing.T) {
	w := newWorld(t)
	w.svc.publishState(context.Background(), w.room(wSecret), w.state(wSecret, wCarol, "s1"), true)
	got := map[string]bool{}
	for _, e := range w.voiceEvents(wSecret) {
		if e.SpaceID == id.Format(wSpace) {
			t.Fatalf("went to the whole space: %+v", e)
		}
		got[e.UserID] = true
	}
	if !got[id.Format(wCarol)] || got[id.Format(wRemote)] {
		t.Fatalf("heard by %v", got)
	}
}

// When nothing can say the room is open, its own channel still reaches its viewers, and no one
// else; a left is never lost for want of a member list.
func TestUnreadableViewersStillReachTheRoom(t *testing.T) {
	w := newWorld(t)
	w.repo.failRead = true
	w.svc.publishState(context.Background(), w.room(wLounge), w.state(wLounge, wCarol, "s1"), false)
	got := w.voiceEvents(wLounge)
	if len(got) != 2 || got[0].SpaceID != id.Format(wLounge) || got[1].UserID != id.Format(wCarol) {
		t.Fatalf("events %+v, want the room's channel and the state's own user", got)
	}
}

// Where someone was moved is the destination's occupancy. Into a private room, only the moved
// user hears it, and before the plain left, so their client rejoins rather than hangs up.
func TestAMoveIntoAPrivateRoomTellsOnlyTheMovedUserWhere(t *testing.T) {
	w := newWorld(t)
	st := w.state(wLounge, wCarol, "s1")
	w.svc.publishMove(context.Background(), w.room(wLounge), st, w.room(wSecret))
	got := w.voiceEvents(wLounge)
	if len(got) != 2 {
		t.Fatalf("events %+v", got)
	}
	if got[0].UserID != id.Format(wCarol) || got[0].Data["moved_to"] != id.Format(wSecret) {
		t.Fatalf("first %+v, want the moved user told where, first", got[0])
	}
	if got[1].SpaceID != id.Format(wSpace) || got[1].Data["moved_to"] != nil || got[1].Data["left"] != true {
		t.Fatalf("second %+v, want a plain left on the space channel", got[1])
	}
}

func TestAMoveIntoAnOpenRoomIsNamedToEveryone(t *testing.T) {
	w := newWorld(t)
	w.svc.publishMove(context.Background(), w.room(wSecret), w.state(wSecret, wAlice, "s1"), w.room(wLounge))
	got := w.voiceEvents(wSecret)
	if len(got) != 2 || got[0].SpaceID != id.Format(wSecret) || got[0].Data["moved_to"] != id.Format(wLounge) || got[1].UserID != id.Format(wAlice) {
		t.Fatalf("events %+v", got)
	}
}

// The relay is what every mirror client builds its media-key targets from: it must not stop at
// the space's own gateway channel.
func TestSpaceStateIsRelayedWhenHostedHere(t *testing.T) {
	w := newWorld(t)
	w.svc.publishState(context.Background(), w.room(wLounge), w.state(wLounge, wCarol, "s1"), false)
	w.svc.publishState(context.Background(), w.room(wSecret), w.state(wSecret, wAlice, "s1"), true)
	want := []string{"state:10:3", "left:12:2"}
	if !eq(w.fed.relays, want) {
		t.Fatalf("relays %v, want %v", w.fed.relays, want)
	}
}

func TestMirrorDoesNotRelayStates(t *testing.T) {
	w := newWorld(t).mirror()
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), w.state(wLounge, wCarol, "s1"), false, 0); err != nil {
		t.Fatal(err)
	}
	if len(w.fed.relays) != 0 {
		t.Fatalf("a mirror relayed %v", w.fed.relays)
	}
}

func TestMoveNamesTheDestination(t *testing.T) {
	w := newWorld(t)
	st := w.put(w.state(wLounge, wRemote, "s1"))
	if err := w.svc.move(context.Background(), st, w.room(wLounge), w.room(wStage)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"moved:10:4:11", "state:11:4"}; !eq(w.fed.relays, want) {
		t.Fatalf("relays %v, want %v", w.fed.relays, want)
	}
	left := w.voiceEvents(wLounge)
	if len(left) != 1 || left[0].Data["moved_to"] != "11" || left[0].Data["left"] != true {
		t.Fatalf("left event %+v", left)
	}
}

// ---- relayed states ------------------------------------------------------------------------

func TestLeftForAnOlderSessionLeavesTheNewOneAlone(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "new"))
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), w.state(wLounge, wAlice, "old"), true, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"2:new"}) {
		t.Fatalf("held %v", got)
	}
	if len(w.voiceEvents(wLounge)) != 0 {
		t.Fatal("a stale left was told to clients, whose call would hang up")
	}
}

func TestLeftCarriesTheLocalDestination(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), w.state(wLounge, wAlice, "s1"), true, wStage); err != nil {
		t.Fatal(err)
	}
	got := w.voiceEvents(wLounge)
	if len(got) != 1 || got[0].Data["moved_to"] != "11" {
		t.Fatalf("events %+v", got)
	}
	if ok := w.held(wLounge); len(ok) != 0 {
		t.Fatalf("still held: %v", ok)
	}
}

func TestLeftWithNoKnownDestinationStillApplies(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), w.state(wLounge, wAlice, "s1"), true, 0); err != nil {
		t.Fatal(err)
	}
	got := w.voiceEvents(wLounge)
	if len(got) != 1 || got[0].Data["moved_to"] != nil || got[0].Data["left"] != true {
		t.Fatalf("events %+v", got)
	}
}

// A relayed state for someone this instance still has in another room moves them: READY reads
// both rooms, and would show them twice.
func TestRelayMovesAUserOutOfTheirOldRoom(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wRemote, "s1"))
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wStage), w.state(wStage, wRemote, "s2"), false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("old room still holds %v", got)
	}
	if got := w.held(wStage); !eq(got, []string{"4:s2"}) {
		t.Fatalf("new room holds %v", got)
	}
	if left := w.voiceEvents(wLounge); len(left) != 1 || left[0].Data["left"] != true {
		t.Fatalf("old room's left: %+v", left)
	}
	if cur, ok, _ := w.store.UserRoom(context.Background(), wRemote); !ok || cur != wStage {
		t.Fatalf("pointer %d %v", cur, ok)
	}
}

// Origins do not relay in step: a late state from one must not undo a newer one from another.
func TestOlderRelayLosesToTheRoomTheUserIsIn(t *testing.T) {
	w := newWorld(t).mirror()
	newer := w.state(wStage, wRemote, "s2")
	w.put(newer)
	stale := w.state(wLounge, wRemote, "s1")
	stale.JoinedAt = newer.JoinedAt.Add(-time.Minute)
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), stale, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wStage); !eq(got, []string{"4:s2"}) {
		t.Fatalf("newer room holds %v", got)
	}
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("stale room holds %v", got)
	}
}

// Bob moved from a room hosted elsewhere to another: a late relay for the old one must not
// repoint him, or /voice/state and /voice/leave lose track of the call he is in.
func TestLateRelayDoesNotRepointOurOwnUser(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wStage, wCarol, "s2"))
	late := w.state(wLounge, wCarol, "s1")
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), late, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("late relay was written: %v", got)
	}
	if cur, _, _ := w.store.UserRoom(context.Background(), wCarol); cur != wStage {
		t.Fatalf("pointer repointed to %d", cur)
	}
}

// A pointer naming a room that holds no state for them is stale, not a call they are in.
func TestStalePointerDoesNotBlockARelay(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wStage, wCarol, "s1"))
	if _, err := w.store.rdb.HDel(context.Background(), w.store.roomKey(wStage), "3").Result(); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wLounge), w.state(wLounge, wCarol, "s2"), false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"3:s2"}) {
		t.Fatalf("held %v", got)
	}
}

// The relay names a room elsewhere while the user's copy here is in a call this instance
// hosts: that state is no copy, and stays until their own instance says they left it.
func TestRelayLeavesARoomHostedHereAlone(t *testing.T) {
	w := newWorld(t)
	w.info.origin[wLounge] = "a.local"
	w.info.origin[wStage] = "b.local"
	w.put(w.state(wLounge, wRemote, "here"))
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wStage), w.state(wStage, wRemote, "there"), false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"4:here"}) {
		t.Fatalf("hosted state dropped: %v", got)
	}
	if got := w.held(wStage); !eq(got, []string{"4:there"}) {
		t.Fatalf("mirrored room holds %v", got)
	}
	// The pointer stays on the call hosted here, or a kick, ban or disconnect stops finding it.
	if cur, _, _ := w.store.UserRoom(context.Background(), wRemote); cur != wLounge {
		t.Fatalf("pointer moved to %d", cur)
	}
}

// One origin's relay does not end a state in a room another origin hosts: that room's own
// relays and pull say when they leave it. Otherwise any peer could take anyone out of a call
// it has nothing to do with, and keep them out with a JoinedAt in the future.
func TestARelayDoesNotEndAnotherOriginsState(t *testing.T) {
	w := newWorld(t).mirror()
	w.info.origin[wLounge] = "c.local"
	w.put(w.state(wLounge, wRemote, "s1"))
	forged := w.state(wStage, wRemote, "s2")
	forged.JoinedAt = time.Now().AddDate(70, 0, 0)
	if err := w.svc.ApplyRemoteVoiceState(context.Background(), w.room(wStage), forged, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"4:s1"}) {
		t.Fatalf("another origin's room holds %v", got)
	}
	if left := w.voiceEvents(wLounge); len(left) != 0 {
		t.Fatalf("published %+v", left)
	}
	if cur, _, _ := w.store.UserRoom(context.Background(), wRemote); cur != wLounge {
		t.Fatalf("pointer moved to %d", cur)
	}
}

func TestLeaveFederatedSparesANewerSession(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.put(w.state(wLounge, wRemote, "s2"))
	if err := w.svc.LeaveFederated(ctx, wRemote, wLounge, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"4:s2"}) {
		t.Fatalf("a queued leave ended the rejoin: %v", got)
	}
	if err := w.svc.LeaveFederated(ctx, wRemote, wLounge, "s2"); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
}

func TestLeaveFederatedFromAnOlderPeerNamesNoSession(t *testing.T) {
	w := newWorld(t)
	w.put(w.state(wLounge, wRemote, "s2"))
	if err := w.svc.LeaveFederated(context.Background(), wRemote, wLounge, ""); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
}

func TestLeavingAMirroredCallNamesTheSession(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wCarol, "s7"))
	if err := w.svc.Leave(context.Background(), wCarol); err != nil {
		t.Fatal(err)
	}
	if want := []string{"10:3:s7"}; !eq(w.fed.leaves, want) {
		t.Fatalf("leaves %v, want %v", w.fed.leaves, want)
	}
}

// ---- the pull ------------------------------------------------------------------------------

// answerWith answers for each room asked about, with the given states.
func answerWith(states map[int64][]State) func([]*rooms.RoomWithParticipants) ([]RoomSync, error) {
	return func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		out := make([]RoomSync, 0, len(list))
		for _, room := range list {
			out = append(out, RoomSync{RoomID: room.ID, States: states[room.ID]})
		}
		return out, nil
	}
}

func TestPullClearsAGhost(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "ghost"))
	w.put(w.state(wLounge, wRemote, "live"))
	w.fed.answer = answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wRemote, "live")}})
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"4:live"}) {
		t.Fatalf("held %v", got)
	}
	got := w.voiceEvents(wLounge)
	if len(got) != 1 || got[0].Data["left"] != true || got[0].Data["user_id"] != "2" {
		t.Fatalf("events %+v, want the ghost's left and nothing about the live state", got)
	}
	if len(w.fed.leaves) != 0 {
		t.Fatalf("forwarded a leave naming someone else's user: %v", w.fed.leaves)
	}
	if cur, ok, _ := w.store.UserRoom(context.Background(), wAlice); ok {
		t.Fatalf("pointer left at %d", cur)
	}
}

func TestPullAddsWhoARelayMissed(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1"), *w.state(wLounge, wRemote, "s9")}})
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "4:s9"}) {
		t.Fatalf("held %v", got)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 || got[0].Data["user_id"] != "4" {
		t.Fatalf("events %+v, want the newcomer: a client keys the states it hears", got)
	}
}

func TestPullRepairsWhatIsStale(t *testing.T) {
	cases := []struct {
		name   string
		change func(*State)
	}{
		{"never connected", func(st *State) { st.Connected = true }},
		{"server mute", func(st *State) { st.Mute = true }},
		{"new session", func(st *State) { st.SessionID, st.Ident = "s2", "@2:a.local.s2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t).mirror()
			have := w.state(wLounge, wAlice, "s1")
			w.put(have)
			want := *have
			tc.change(&want)
			w.fed.answer = answerWith(map[int64][]State{wLounge: {want}})
			w.sweep()
			if got := w.voiceEvents(wLounge); len(got) != 1 {
				t.Fatalf("events %+v", got)
			}
		})
	}
}

// Rows written before mirrored states had a space: the first pull gives them one.
func TestPullRewritesARowWithNoSpace(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	old := w.state(wLounge, wAlice, "s1")
	old.SpaceID = 0
	w.put(old)
	w.fed.answer = answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})
	w.sweep()
	st, _ := w.store.GetState(ctx, wLounge, wAlice)
	if st == nil || st.SpaceID != wSpace {
		t.Fatalf("state %+v", st)
	}
}

func TestPullLeavesWhatIsRightAlone(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	have := w.put(w.state(wLounge, wAlice, "s1"))
	want := *have
	want.IssuedAt = time.Now().UTC()
	w.fed.answer = answerWith(map[int64][]State{wLounge: {want}})
	w.sweep()
	if got := w.voiceEvents(wLounge); len(got) != 0 {
		t.Fatalf("events %+v", got)
	}
	st, _ := w.store.GetState(ctx, wLounge, wAlice)
	if !st.IssuedAt.Equal(have.IssuedAt) {
		t.Fatal("rewrote an unchanged state")
	}
}

// A relay landing while the answer is in flight is newer than the answer.
func TestPullDoesNotOverwriteARelayThatLandedMeanwhile(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		st := w.state(wLounge, wAlice, "s1")
		st.Mute = true
		time.Sleep(time.Millisecond)
		if err := w.svc.ApplyRemoteVoiceState(ctx, list[0], st, false, 0); err != nil {
			t.Fatal(err)
		}
		return answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})(list)
	}
	w.sweep()
	st, _ := w.store.GetState(ctx, wLounge, wAlice)
	if st == nil || !st.Mute {
		t.Fatalf("state %+v, want the relay's", st)
	}
}

// Someone who joined while the answer was in flight is not in it, and is not a ghost.
func TestPullDoesNotDropAJoinThatLandedMeanwhile(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		time.Sleep(time.Millisecond)
		if err := w.svc.ApplyRemoteVoiceState(ctx, list[0], w.state(wLounge, wCarol, "s2"), false, 0); err != nil {
			t.Fatal(err)
		}
		return answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})(list)
	}
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "3:s2"}) {
		t.Fatalf("held %v", got)
	}
}

// The answer names someone a left relay has just removed: bringing them back makes a ghost, and
// a ghost is a media-key target.
func TestPullDoesNotResurrectWhoLeftMeanwhile(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		if err := w.svc.ApplyRemoteVoiceState(ctx, list[0], w.state(wLounge, wAlice, "s1"), true, 0); err != nil {
			t.Fatal(err)
		}
		return answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})(list)
	}
	w.sweep()
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
}

func TestPullGoneClearsAtOnceAndStopsAsking(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		return []RoomSync{{RoomID: wLounge, Gone: true}}, nil
	}
	w.sweep()
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 || got[0].Data["left"] != true {
		t.Fatalf("events %+v", got)
	}
	if w.active(wLounge) {
		t.Fatal("a gone room is still asked about")
	}
}

// One state that could not be read: being unnamed proves nothing about anyone.
func TestIncompleteAnswerRemovesNobody(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		return []RoomSync{{RoomID: wLounge, Incomplete: true, States: []State{*w.state(wLounge, wCarol, "s2")}}}, nil
	}
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "3:s2"}) {
		t.Fatalf("held %v", got)
	}
}

// A room the answer leaves out (an origin near its size limit) is left alone, and listed.
func TestRoomLeftOutOfTheAnswerIsUntouched(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func([]*rooms.RoomWithParticipants) ([]RoomSync, error) { return []RoomSync{}, nil }
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("held %v", got)
	}
}

func TestEmptyRoomLeavesTheListOnlyOnAnEmptyAnswer(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	if err := w.store.TrackRoom(ctx, wLounge); err != nil {
		t.Fatal(err)
	}
	w.fed.answer = func([]*rooms.RoomWithParticipants) ([]RoomSync, error) { return nil, errors.New("timeout") }
	w.sweep()
	if !w.active(wLounge) {
		t.Fatal("a failed question took the room off the list")
	}
	w.mem.DeleteString(w.store.syncKey("retry", "b.local"))
	w.fed.answer = answerWith(nil)
	w.sweep()
	if w.active(wLounge) {
		t.Fatal("an empty answer should end the asking")
	}
}

func TestOlderOriginIsLeftAloneAndItsStatesKept(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = func([]*rooms.RoomWithParticipants) ([]RoomSync, error) { return nil, ErrOriginPredatesSync }
	w.sweep()
	w.sweep()
	if len(w.fed.asks) != 1 {
		t.Fatalf("asked %d times", len(w.fed.asks))
	}
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("held %v", got)
	}
	// Not an outage: nothing is counting down to clearing its calls.
	if w.mem.HasString(w.store.syncKey("down", "b.local")) {
		t.Fatal("an older origin was counted as down")
	}
}

func TestOriginThatStopsAnsweringKeepsItsStatesThenLosesThem(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	calls := 0
	w.fed.answer = func([]*rooms.RoomWithParticipants) ([]RoomSync, error) {
		calls++
		return nil, errors.New("connection refused")
	}
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("a blip cleared a live call: %v", got)
	}
	w.sweep()
	if calls != 1 {
		t.Fatalf("asked again %d time(s) within the minute", calls-1)
	}

	// Silent for longer than the limit: the next failure ends it, and the room stays listed.
	w.mem.DeleteString(w.store.syncKey("retry", "b.local"))
	w.mem.SetString(w.store.syncKey("down", "b.local"), time.Now().Add(-originSilenceLimit-time.Second).UTC().Format(time.RFC3339Nano))
	w.sweep()
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 || got[0].Data["left"] != true {
		t.Fatalf("events %+v", got)
	}
	if !w.active(wLounge) {
		t.Fatal("the room must stay listed so the first answer can restore it")
	}

	// It comes back: everyone still there is restored, and the outage is forgotten.
	w.mem.DeleteString(w.store.syncKey("retry", "b.local"))
	w.fed.answer = answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("held %v", got)
	}
	if w.mem.HasString(w.store.syncKey("down", "b.local")) {
		t.Fatal("the outage was not forgotten")
	}
}

func TestOneReplicaAsks(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.fed.answer = answerWith(map[int64][]State{wLounge: {*w.state(wLounge, wAlice, "s1")}})
	ctx := context.Background()
	w.svc.reconcile(ctx)
	w.svc.reconcile(ctx)
	if len(w.fed.asks) != 1 {
		t.Fatalf("asked %d times in one sweep", len(w.fed.asks))
	}
}

func TestPullAsksInBatches(t *testing.T) {
	w := newWorld(t).mirror()
	space := int64(wSpace)
	for i := range MaxSyncRooms + 1 {
		rid := int64(1000 + i)
		w.rooms.byID[rid] = &rooms.Room{ID: rid, Type: rooms.TypeSpaceVoice, SpaceID: &space}
		w.info.origin[rid] = "b.local"
		w.put(w.state(rid, int64(2000+i), "s"))
	}
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) { return []RoomSync{}, nil }
	w.sweep()
	if len(w.fed.asks) != 2 || len(w.fed.asks[0]) != MaxSyncRooms || len(w.fed.asks[1]) != 1 {
		t.Fatalf("asks %v", w.fed.asks)
	}
}

// An origin that is down must not delay another, or the local sweep.
func TestOriginsAreAskedSeparately(t *testing.T) {
	w := newWorld(t).mirror()
	w.info.origin[wStage] = "c.local"
	w.info.origin[wSecret] = "a.local"
	w.put(w.state(wLounge, wAlice, "s1"))
	w.put(w.state(wStage, wCarol, "s1"))
	// Neither answers until both have been asked: in series this would time out.
	asked := make(chan struct{}, 2)
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		asked <- struct{}{}
		deadline := time.After(2 * time.Second)
		for len(asked) < 2 {
			select {
			case <-deadline:
				t.Error("the other origin was never asked")
				return nil, errors.New("timeout")
			case <-time.After(time.Millisecond):
			}
		}
		if list[0].ID == wLounge {
			return nil, errors.New("timeout")
		}
		return answerWith(nil)(list)
	}
	w.sweep()
	if got := w.held(wStage); len(got) != 0 {
		t.Fatalf("c.local's room held %v", got)
	}
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("b.local's room held %v", got)
	}
}

func TestLostCallDeleteIsRecovered(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	call := &Call{RoomID: wLounge, StartedBy: wAlice, StartedAt: time.Now().UTC()}
	if err := w.svc.ApplyRemoteCall(ctx, w.room(wLounge), EventCallCreate, call, nil, false); err != nil {
		t.Fatal(err)
	}
	if !w.active(wLounge) {
		t.Fatal("a mirrored call must be listed, or nothing ever asks about it")
	}
	w.fed.answer = answerWith(nil)
	w.sweep()
	if got, _ := w.store.GetCall(ctx, wLounge); got != nil {
		t.Fatalf("call %+v", got)
	}
	if w.active(wLounge) {
		t.Fatal("still listed")
	}
}

func TestCallKeepsTheRoomListedWhenItsLastStateGoes(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	if err := w.store.PutCall(ctx, &Call{RoomID: wLounge, StartedBy: wAlice, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.ApplyRemoteVoiceState(ctx, w.room(wLounge), w.state(wLounge, wAlice, "s1"), true, 0); err != nil {
		t.Fatal(err)
	}
	if !w.active(wLounge) {
		t.Fatal("the last leave took the room off the list with a call still in it")
	}
}

// A call the answer does not name, but that started after the question went out, stands.
func TestCallStartedMeanwhileIsKept(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	old := &Call{RoomID: wLounge, StartedBy: wAlice, StartedAt: time.Now().UTC().Add(-time.Hour)}
	if err := w.svc.ApplyRemoteCall(ctx, w.room(wLounge), EventCallCreate, old, nil, false); err != nil {
		t.Fatal(err)
	}
	newer := &Call{RoomID: wLounge, StartedBy: wCarol, StartedAt: time.Now().UTC()}
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		if err := w.store.PutCall(ctx, newer); err != nil {
			t.Fatal(err)
		}
		return answerWith(nil)(list)
	}
	w.sweep()
	got, _ := w.store.GetCall(ctx, wLounge)
	if got == nil || got.StartedBy != wCarol {
		t.Fatalf("call %+v", got)
	}
}

func TestRunningCallIsNotEnded(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	call := &Call{RoomID: wLounge, StartedBy: wAlice, StartedAt: time.Now().UTC()}
	if err := w.svc.ApplyRemoteCall(ctx, w.room(wLounge), EventCallCreate, call, nil, false); err != nil {
		t.Fatal(err)
	}
	w.fed.answer = func(list []*rooms.RoomWithParticipants) ([]RoomSync, error) {
		return []RoomSync{{RoomID: wLounge, Call: call}}, nil
	}
	w.sweep()
	if got, _ := w.store.GetCall(ctx, wLounge); got == nil {
		t.Fatal("ended a call the origin still has")
	}
	if !w.active(wLounge) {
		t.Fatal("a running call stopped being asked about")
	}
}

// ---- the sweep's classification ------------------------------------------------------------

// A room read that fails says nothing: treating the room as local asks this instance's LiveKit,
// which has never heard of a mirrored call and would report everyone gone.
func TestFailedRoomReadSkipsTheRoom(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.rooms.failFor[wLounge] = true
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("held %v", got)
	}
	if w.lk.listed != 0 {
		t.Fatal("asked LiveKit about a room it could not read")
	}
}

// The mapping read fails without saying so: the room just looks unfederated. A federated
// identity in it gives that away.
func TestUnreadableMappingSkipsTheRoom(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	delete(w.info.origin, wLounge)
	w.sweep()
	if got := w.held(wLounge); !eq(got, []string{"2:s1"}) {
		t.Fatalf("held %v", got)
	}
}

func TestDeletedRoomIsStillSwept(t *testing.T) {
	w := newWorld(t).mirror()
	st := w.state(wLounge, wAlice, "s1")
	w.put(st)
	delete(w.rooms.byID, wLounge)
	w.sweep()
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if len(w.fed.asks) != 0 {
		t.Fatalf("asked about a room that is gone: %v", w.fed.asks)
	}
}

func TestHostedRoomKeepsItsLiveKitSweep(t *testing.T) {
	w := newWorld(t)
	w.put(w.state(wLounge, wAlice, "gone"))
	live := w.put(w.state(wStage, wCarol, "live"))
	w.lk.present[RoomName(wStage)] = []string{live.Identity(), "9.intruder"}
	w.sweep()
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if got := w.held(wStage); !eq(got, []string{"3:live"}) {
		t.Fatalf("held %v", got)
	}
	if !eq(w.lk.removed, []string{"9.intruder"}) {
		t.Fatalf("removed %v", w.lk.removed)
	}
	if len(w.fed.asks) != 0 {
		t.Fatal("asked an origin about a room hosted here")
	}
}

// ---- joining through the origin ------------------------------------------------------------

func joinAs(w *world, user *auth.User, others ...State) *JoinResult {
	w.t.Helper()
	mine := w.state(wLounge, user.ID, "mine")
	states := append([]State{}, others...)
	states = append(states, *mine)
	res := &JoinResult{State: mine, States: states}
	w.fed.join = func() (*RemoteJoin, error) { return &RemoteJoin{Result: res}, nil }
	got, err := w.svc.joinViaOrigin(context.Background(), user, w.room(wLounge), "b.local", JoinInput{})
	if err != nil {
		w.t.Fatal(err)
	}
	return got
}

func TestJoinReplyAnnouncesWhoItFinds(t *testing.T) {
	w := newWorld(t).mirror()
	joinAs(w, w.users.byID[wCarol], *w.state(wLounge, wAlice, "s1"))
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "3:mine"}) {
		t.Fatalf("held %v", got)
	}
	heard := map[any]bool{}
	for _, e := range w.voiceEvents(wLounge) {
		heard[e.Data["user_id"]] = true
	}
	if !heard["2"] || !heard["3"] {
		t.Fatalf("heard %v", heard)
	}
}

func TestJoinReplyDoesNotResurrectWhoLeft(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	gone := w.state(wLounge, wAlice, "s1")
	user := w.users.byID[wCarol]
	mine := w.state(wLounge, wCarol, "mine")
	w.fed.join = func() (*RemoteJoin, error) {
		// The left relay reaches this instance ahead of the reply that still names her.
		if err := w.svc.ApplyRemoteVoiceState(ctx, w.room(wLounge), gone, true, 0); err != nil {
			t.Fatal(err)
		}
		return &RemoteJoin{Result: &JoinResult{State: mine, States: []State{*gone, *mine}}}, nil
	}
	if _, err := w.svc.joinViaOrigin(ctx, user, w.room(wLounge), "b.local", JoinInput{}); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"3:mine"}) {
		t.Fatalf("held %v", got)
	}
}

func TestJoinReplyDoesNotOverwriteARelay(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	user := w.users.byID[wCarol]
	mine := w.state(wLounge, wCarol, "mine")
	w.fed.join = func() (*RemoteJoin, error) {
		muted := w.state(wLounge, wAlice, "s1")
		muted.Mute = true
		time.Sleep(time.Millisecond)
		if err := w.svc.ApplyRemoteVoiceState(ctx, w.room(wLounge), muted, false, 0); err != nil {
			t.Fatal(err)
		}
		return &RemoteJoin{Result: &JoinResult{State: mine, States: []State{*w.state(wLounge, wAlice, "s1"), *mine}}}, nil
	}
	if _, err := w.svc.joinViaOrigin(ctx, user, w.room(wLounge), "b.local", JoinInput{}); err != nil {
		t.Fatal(err)
	}
	st, _ := w.store.GetState(ctx, wLounge, wAlice)
	if st == nil || !st.Mute {
		t.Fatalf("state %+v", st)
	}
}

// Removal is the pull's: a reply can be older than what is here.
func TestJoinReplyRemovesNobody(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	joinAs(w, w.users.byID[wCarol])
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "3:mine"}) {
		t.Fatalf("held %v", got)
	}
}

// The caller's own state is written whatever else is true.
func TestJoinReplyAlwaysWritesTheCaller(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	user := w.users.byID[wCarol]
	w.put(w.state(wLounge, wCarol, "old"))
	mine := w.state(wLounge, wCarol, "new")
	w.fed.join = func() (*RemoteJoin, error) {
		time.Sleep(time.Millisecond)
		if err := w.svc.ApplyRemoteVoiceState(ctx, w.room(wLounge), w.state(wLounge, wCarol, "relayed"), false, 0); err != nil {
			t.Fatal(err)
		}
		return &RemoteJoin{Result: &JoinResult{State: mine, States: []State{*mine}}}, nil
	}
	if _, err := w.svc.joinViaOrigin(ctx, user, w.room(wLounge), "b.local", JoinInput{}); err != nil {
		t.Fatal(err)
	}
	if got := w.held(wLounge); !eq(got, []string{"3:new"}) {
		t.Fatalf("held %v", got)
	}
}

// ---- snapshot discovery ----------------------------------------------------------------------

func TestSnapshotRevealsARunningCallButRemovesNothing(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wAlice, "s1"))
	w.svc.ApplySnapshotStates(ctx, w.room(wLounge), []State{*w.state(wLounge, wCarol, "s2")}, time.Now())
	if got := w.held(wLounge); !eq(got, []string{"2:s1", "3:s2"}) {
		t.Fatalf("held %v", got)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 {
		t.Fatalf("events %+v", got)
	}
}

func TestLiveStatesGroupsByRoom(t *testing.T) {
	w := newWorld(t)
	w.put(w.state(wLounge, wAlice, "s1"))
	w.put(w.state(wLounge, wCarol, "s2"))
	got, err := w.svc.LiveStates(context.Background(), []int64{wLounge, wStage})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[wLounge]) != 2 || len(got[wStage]) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// ---- moderation ------------------------------------------------------------------------------

func TestModerationOfAMirroredSpaceGoesToTheOrigin(t *testing.T) {
	ctx := context.Background()
	mute := true
	actor := &auth.User{ID: wAlice}
	cases := []struct {
		name string
		call func(w *world) error
		want string
	}{
		{"mute", func(w *world) error { return w.svc.Moderate(ctx, actor, wSpace, wCarol, ModerateInput{Mute: &mute}) }, "moderate:3"},
		{"disconnect", func(w *world) error { return w.svc.Disconnect(ctx, actor, wSpace, wCarol) }, "disconnect:3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Whether this instance has a copy of the state or not.
			for _, copied := range []bool{false, true} {
				w := newWorld(t).mirror()
				if copied {
					w.put(w.state(wLounge, wCarol, "s1"))
				}
				if err := tc.call(w); err != nil {
					t.Fatal(err)
				}
				if !eq(w.fed.forward, []string{tc.want}) {
					t.Fatalf("forwarded %v", w.fed.forward)
				}
				if len(w.lk.removed) != 0 || len(w.events()) != 0 {
					t.Fatal("acted here as well")
				}
				if copied && !eq(w.held(wLounge), []string{"3:s1"}) {
					t.Fatal("a refused or pending moderation changed the mirror")
				}
			}
		})
	}
}

func TestOriginsRefusalReachesTheModerator(t *testing.T) {
	w := newWorld(t).mirror()
	w.fed.reply = &OriginError{Status: 403, Message: "missing permission"}
	mute := true
	err := w.svc.Moderate(context.Background(), &auth.User{ID: wAlice}, wSpace, wCarol, ModerateInput{Mute: &mute})
	var oe *OriginError
	if !errors.As(err, &oe) || oe.Status != 403 {
		t.Fatalf("err %v", err)
	}
}

func TestModerationOfAnEmptyPatchNeverLeaves(t *testing.T) {
	w := newWorld(t).mirror()
	err := w.svc.Moderate(context.Background(), &auth.User{ID: wAlice}, wSpace, wCarol, ModerateInput{})
	if !errors.Is(err, spaces.ErrNothingToPatch) || len(w.fed.forward) != 0 {
		t.Fatalf("err %v, forwarded %v", err, w.fed.forward)
	}
}

// ---- members and rooms going -----------------------------------------------------------------

// The origin already ended the connection, and a kicked member of a third instance is no user
// of this one to speak for: leave() would forward a leave naming them and get a 403.
func TestMemberRemovedFromAMirrorDropsTheCopyOnly(t *testing.T) {
	w := newWorld(t).mirror()
	st := w.state(wLounge, wRemote, "s1")
	st.SpaceID = 0 // a row from before mirrored states had a space
	w.put(st)
	w.svc.OnSpaceMemberRemoved(context.Background(), wSpace, wRemote)
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if len(w.fed.leaves) != 0 {
		t.Fatalf("forwarded %v", w.fed.leaves)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 || got[0].Data["left"] != true {
		t.Fatalf("events %+v", got)
	}
}

func TestMemberRemovedFromOneSpaceKeepsACallInAnother(t *testing.T) {
	w := newWorld(t)
	w.put(w.state(wLounge, wCarol, "s1"))
	w.svc.OnSpaceMemberRemoved(context.Background(), wSpace+1, wCarol)
	if got := w.held(wLounge); !eq(got, []string{"3:s1"}) {
		t.Fatalf("held %v", got)
	}
}

func TestMemberRemovedHereHangsThemUp(t *testing.T) {
	w := newWorld(t)
	st := w.put(w.state(wLounge, wCarol, "s1"))
	w.svc.OnSpaceMemberRemoved(context.Background(), wSpace, wCarol)
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if !eq(w.lk.removed, []string{st.Identity()}) {
		t.Fatalf("removed %v", w.lk.removed)
	}
}

// Losing View of a room ends a call in it, here where the call runs: the rest of the room stays.
func TestLosingViewHangsUpOnlyThoseWhoLostIt(t *testing.T) {
	w := newWorld(t)
	carol := w.put(w.state(wSecret, wCarol, "s1"))
	w.put(w.state(wSecret, wAlice, "s2"))
	w.svc.OnRoomAccessLost(context.Background(), wSpace, wSecret, []int64{wCarol, wRemote})
	if got := w.held(wSecret); !eq(got, []string{"2:s2"}) {
		t.Fatalf("held %v", got)
	}
	if !eq(w.lk.removed, []string{carol.Identity()}) {
		t.Fatalf("removed %v", w.lk.removed)
	}
}

// A mirror holds a copy of a call another instance runs: that instance ends it and says so.
func TestLosingViewOnAMirrorLeavesItToTheOrigin(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wSecret, wRemote, "s1"))
	w.svc.OnRoomAccessLost(context.Background(), wSpace, wSecret, []int64{wRemote})
	if got := w.held(wSecret); !eq(got, []string{"4:s1"}) {
		t.Fatalf("held %v", got)
	}
	if len(w.fed.leaves) != 0 || len(w.lk.removed) != 0 {
		t.Fatalf("forwarded %v, removed %v", w.fed.leaves, w.lk.removed)
	}
}

func TestDeletingARoomEndsItsCall(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	st := w.put(w.state(wLounge, wCarol, "s1"))
	w.svc.OnSpaceRoomRemoved(ctx, wSpace, wLounge)
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if cur, ok, _ := w.store.UserRoom(ctx, wCarol); ok {
		t.Fatalf("pointer at %d", cur)
	}
	if !eq(w.lk.removed, []string{st.Identity()}) {
		t.Fatalf("removed %v", w.lk.removed)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 || got[0].Data["left"] != true {
		t.Fatalf("events %+v", got)
	}
	if !eq(w.fed.relays, []string{"left:10:3"}) {
		t.Fatalf("relays %v", w.fed.relays)
	}
	if w.active(wLounge) {
		t.Fatal("still listed")
	}
}

func TestDeletingAMirroredRoomTouchesNoLiveKit(t *testing.T) {
	w := newWorld(t).mirror()
	w.put(w.state(wLounge, wCarol, "s1"))
	w.svc.OnSpaceRoomRemoved(context.Background(), wSpace, wLounge)
	if got := w.held(wLounge); len(got) != 0 {
		t.Fatalf("held %v", got)
	}
	if len(w.lk.removed) != 0 || len(w.fed.relays) != 0 {
		t.Fatalf("removed %v, relays %v", w.lk.removed, w.fed.relays)
	}
	if got := w.voiceEvents(wLounge); len(got) != 1 {
		t.Fatalf("events %+v", got)
	}
}

func TestDeletingAnEmptyRoomIsNothing(t *testing.T) {
	w := newWorld(t)
	w.svc.OnSpaceRoomRemoved(context.Background(), wSpace, wLounge)
	if len(w.events()) != 0 || len(w.lk.removed) != 0 {
		t.Fatal("did something for an empty room")
	}
}
