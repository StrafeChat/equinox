package messages

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/StrafeChat/equinox/internal/modules/permissions"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/safego"
)

// SearchOptions is a message-search query, roughly Discord's search operators. Query is
// matched as a case-insensitive substring against plaintext - there is no full-text index,
// so this is an in-partition scan (see SearchMessages), which is the honest tradeoff for a
// self-hosted app at this scale rather than a mandatory search-index dependency.
type SearchOptions struct {
	Query          string
	FromUserID     *int64
	MentionsUserID *int64
	// Has: "", "link", "image", "video", "audio", or "file" (any attachment).
	Has      string
	BeforeID *int64
	Limit    int
}

// SearchResult is one page of matches. Searchable is false for E2EE rooms - the server
// never sees their plaintext, so no scan is attempted; the client falls back to searching
// its own decrypted history instead.
type SearchResult struct {
	Messages     []Message
	NextBeforeID *int64
	Searchable   bool
}

const (
	// searchPageSize is how many rows are pulled from Scylla per underlying List call
	// while scanning for matches.
	searchPageSize = 100
	// searchScanCap bounds worst-case latency on a huge, mostly-non-matching room: stop
	// after scanning this many messages even if the limit hasn't been reached, and report
	// a cursor so the client can ask for "more" (a deliberate incremental-search UX rather
	// than an unbounded server-side scan per request).
	searchScanCap = 2000
	// spaceScanCap is the same budget for a whole-space search, split across its channels.
	spaceScanCap = 6000
	// minRoomScanCap keeps every channel worth searching even in a space with many of them.
	minRoomScanCap = 250
	// searchConcurrency is how many channels a space-wide search scans at once.
	searchConcurrency = 8
)

var searchLinkRe = regexp.MustCompile(`https?://`)

func matchesHas(atts []Attachment, plaintext, has string) bool {
	switch has {
	case "":
		return true
	case "link":
		return searchLinkRe.MatchString(plaintext)
	case "file":
		return len(atts) > 0
	case "image", "video", "audio":
		prefix := has + "/"
		for _, a := range atts {
			if strings.HasPrefix(a.ContentType, prefix) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func matchesSearch(m *Message, opts *SearchOptions, q string) bool {
	if opts.FromUserID != nil && m.SenderID != *opts.FromUserID {
		return false
	}
	if opts.MentionsUserID != nil {
		found := false
		for _, uid := range m.Mentions {
			if uid == *opts.MentionsUserID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !matchesHas(m.Attachments(), m.Plaintext, opts.Has) {
		return false
	}
	if q != "" && !strings.Contains(strings.ToLower(m.Plaintext), q) {
		return false
	}
	return true
}

// SearchMessages scans a room's history for messages matching opts, newest first. Only
// plaintext (non-E2EE) rooms are searchable server-side; E2EE rooms return
// {Searchable: false} immediately; the client already holds (or can fetch and decrypt)
// its own copy of history to search locally.
func (s *Service) SearchMessages(ctx context.Context, userID, roomID int64, opts *SearchOptions) (*SearchResult, error) {
	room, _, err := s.authorize(ctx, userID, roomID, permissions.PermViewRoom, permissions.PermReadMessageHistory)
	if err != nil {
		return nil, err
	}
	if !roomE2EEOff(room) {
		return &SearchResult{Searchable: false}, nil
	}
	limit := searchLimit(opts.Limit)
	q := strings.ToLower(strings.TrimSpace(opts.Query))
	out, floor, err := s.scanRoom(ctx, roomID, opts, q, opts.BeforeID, limit, searchScanCap)
	if err != nil {
		return nil, err
	}
	var nextBefore *int64
	if floor != 0 {
		nextBefore = &floor
	}
	return &SearchResult{Messages: out, NextBeforeID: nextBefore, Searchable: true}, nil
}

func searchLimit(n int) int {
	if n <= 0 || n > 50 {
		return 25
	}
	return n
}

// scanRoom walks one room's history newest-first (starting below `before` when given),
// collecting up to `limit` matches while examining at most `scanCap` messages.
//
// `floor` is the oldest message id this scan actually examined, i.e. the result is complete
// for every id >= floor; 0 means the room was scanned all the way back to its first message.
// Callers merging several rooms rely on that: max(floor) across rooms is the point below
// which the merged list can no longer be trusted to be in order, and the cursor to resume
// from.
func (s *Service) scanRoom(ctx context.Context, roomID int64, opts *SearchOptions, q string, before *int64, limit, scanCap int) ([]Message, int64, error) {
	var out []Message
	cursor := before
	scanned := 0
	for scanned < scanCap {
		batch, err := s.repo.List(ctx, roomID, cursor, searchPageSize)
		if err != nil {
			return nil, 0, err
		}
		if len(batch) == 0 {
			return out, 0, nil
		}
		for i := range batch {
			scanned++
			m := &batch[i]
			last := m.ID
			cursor = &last
			if matchesSearch(m, opts, q) {
				out = append(out, *m)
				if len(out) >= limit {
					return out, m.ID, nil
				}
			}
		}
		if len(batch) < searchPageSize {
			// Partition exhausted: nothing further back, whatever we have is final.
			return out, 0, nil
		}
	}
	if cursor != nil {
		// Hit the scan cap without exhausting the room or filling the page - there may be
		// more (unscanned) history; let the caller resume from here.
		return out, *cursor, nil
	}
	return out, 0, nil
}

// SpaceSearchOptions is a space-wide search: the same operators as SearchOptions, plus
// Discord's `in:` - an optional restriction to a single channel of the space.
type SpaceSearchOptions struct {
	SearchOptions
	RoomID *int64
}

// SpaceSearchResult is one page of matches from across a space's text channels, newest
// first. Every Message carries its own RoomID so the client can group results by channel.
type SpaceSearchResult struct {
	Messages     []Message
	NextBeforeID *int64
	// RoomsSearched / EncryptedRooms let the client say honestly how complete the result
	// set is: an E2EE channel is never scanned, because the server cannot read it.
	RoomsSearched  int
	EncryptedRooms int
}

// SearchSpaceMessages searches every text channel of a space the caller can read, merged
// newest-first. E2EE channels are skipped (counted in EncryptedRooms) - the server holds
// only their ciphertext.
func (s *Service) SearchSpaceMessages(ctx context.Context, userID, spaceID int64, opts *SpaceSearchOptions) (*SpaceSearchResult, error) {
	if s.spaceAuth == nil {
		return nil, ErrForbidden
	}
	member, err := s.spaceAuth.IsMember(ctx, spaceID, userID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotParticipant
	}
	rows, err := s.rooms.ListBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if opts.RoomID != nil && r.RoomID != *opts.RoomID {
			continue
		}
		ids = append(ids, r.RoomID)
	}
	if len(ids) == 0 {
		return &SpaceSearchResult{}, nil
	}
	loaded, err := s.rooms.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var targets []int64
	encrypted := 0
	for _, room := range loaded {
		if room == nil || room.Type != rooms.TypeSpaceText {
			continue
		}
		if !roomE2EEOff(room) {
			encrypted++
			continue
		}
		perms, err := s.spaceAuth.EffectiveChannelPermissions(ctx, userID, spaceID, room.ID)
		if err != nil {
			continue
		}
		if !permissions.Has(perms, permissions.PermViewRoom) || !permissions.Has(perms, permissions.PermReadMessageHistory) {
			continue
		}
		targets = append(targets, room.ID)
	}
	if len(targets) == 0 {
		return &SpaceSearchResult{EncryptedRooms: encrypted}, nil
	}

	limit := searchLimit(opts.Limit)
	q := strings.ToLower(strings.TrimSpace(opts.Query))
	perRoomCap := spaceScanCap / len(targets)
	if perRoomCap < minRoomScanCap {
		perRoomCap = minRoomScanCap
	}

	type scan struct {
		msgs  []Message
		floor int64
		err   error
	}
	out := make([]scan, len(targets))
	sem := make(chan struct{}, searchConcurrency)
	var wg sync.WaitGroup
	for i, rid := range targets {
		wg.Add(1)
		go safego.Run("messages", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			msgs, floor, err := s.scanRoom(ctx, rid, &opts.SearchOptions, q, opts.BeforeID, limit, perRoomCap)
			out[i] = scan{msgs: msgs, floor: floor, err: err}
		})
	}
	wg.Wait()

	var merged []Message
	var maxFloor int64
	for _, r := range out {
		if r.err != nil {
			return nil, r.err
		}
		merged = append(merged, r.msgs...)
		if r.floor > maxFloor {
			maxFloor = r.floor
		}
	}
	sort.Slice(merged, func(a, b int) bool { return merged[a].ID > merged[b].ID })
	// Below maxFloor at least one channel is unscanned, so the merged order is no longer
	// trustworthy - cut there and let the client resume from that point.
	kept := merged[:0]
	for _, m := range merged {
		if m.ID < maxFloor {
			break
		}
		kept = append(kept, m)
	}
	merged = kept

	var nextBefore *int64
	if len(merged) > limit {
		merged = merged[:limit]
		last := merged[len(merged)-1].ID
		nextBefore = &last
	} else if maxFloor != 0 {
		nextBefore = &maxFloor
	}
	return &SpaceSearchResult{
		Messages:       merged,
		NextBeforeID:   nextBefore,
		RoomsSearched:  len(targets),
		EncryptedRooms: encrypted,
	}, nil
}
