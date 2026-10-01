package messages

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scylladb/gocqlx/v3/table"

	"github.com/StrafeChat/equinox/internal/id"
)

var reactionsTable = table.New(table.Metadata{
	Name:    "message_reactions",
	Columns: []string{"room_id", "message_id", "emoji", "user_id", "created_at"},
	PartKey: []string{"room_id", "message_id"},
	SortKey: []string{"emoji", "user_id"},
})

// Reaction is one (message, emoji, user) row - who reacted to a message with what.
type Reaction struct {
	RoomID    int64     `db:"room_id"`
	MessageID int64     `db:"message_id"`
	Emoji     string    `db:"emoji"`
	UserID    int64     `db:"user_id"`
	CreatedAt time.Time `db:"created_at"`
}

// ReactionSummary is the aggregated wire form of one emoji on one message: how many
// people reacted, and whether the requesting user is one of them.
type ReactionSummary struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Me    bool   `json:"me"`
}

const (
	// MaxReactionsPerMessage caps how many *distinct* emoji one message may carry (matches
	// Discord's own limit) - not how many people may react with an existing one.
	MaxReactionsPerMessage = 20
	customEmojiPrefix      = "custom:"
)

var (
	ErrInvalidReaction  = errors.New("invalid reaction emoji")
	ErrTooManyReactions = errors.New("this message already has the maximum number of different reactions")
)

// unicodeReactionRe is deliberately permissive - it rejects control characters, digits and
// plain ASCII letters (so "custom:123" without the prefix, or a typo, can't sneak through
// as a "unicode" reaction) while accepting emoji, ZWJ sequences, skin-tone modifiers and
// variation selectors, without maintaining a full Unicode emoji property table server-side.
var unicodeReactionRe = regexp.MustCompile(`^[^\x00-\x7F]+$`)

// keycapReactionRe matches the keycap emoji (0️⃣-9️⃣, #️⃣, *️⃣) - the only emoji that begin
// with an ASCII character: a digit, '#' or '*', then the enclosing-keycap mark (U+20E3),
// optionally with the emoji variation selector (U+FE0F) between them. unicodeReactionRe's
// all-non-ASCII rule rejects them on that leading ASCII byte, so they are accepted here
// explicitly rather than by loosening that rule (which is what keeps a bare ASCII typo out).
var keycapReactionRe = regexp.MustCompile(`^[0-9#*]\x{FE0F}?\x{20E3}$`)

// ValidateReactionEmoji normalises and checks a client-supplied emoji key: either a bare
// unicode emoji, or "custom:<id>" referencing a space's custom emoji. Existence of a
// custom emoji isn't verified here - the same trust level the message composer already
// gives an emoji embedded in content (see CreateMessageInput's Mentions doc for the same
// tradeoff), and its image/name only matter for rendering, which the frontend resolves
// lazily via GET /emojis/:id regardless of who's a member of the home space.
func ValidateReactionEmoji(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidReaction
	}
	if strings.HasPrefix(raw, customEmojiPrefix) {
		idPart := strings.TrimPrefix(raw, customEmojiPrefix)
		if _, err := id.Parse(idPart); err != nil {
			return "", ErrInvalidReaction
		}
		return raw, nil
	}
	if len(raw) > 64 || !(unicodeReactionRe.MatchString(raw) || keycapReactionRe.MatchString(raw)) {
		return "", ErrInvalidReaction
	}
	return raw, nil
}

// summarize aggregates raw reaction rows into the wire form, ordered by when each emoji
// was first added (matches Discord's reaction-bar ordering, not alphabetical).
func summarize(rows []Reaction, viewerID int64) []ReactionSummary {
	type agg struct {
		emoji     string
		count     int
		me        bool
		firstTime time.Time
	}
	byEmoji := make(map[string]*agg)
	var order []*agg
	for _, r := range rows {
		a, ok := byEmoji[r.Emoji]
		if !ok {
			a = &agg{emoji: r.Emoji, firstTime: r.CreatedAt}
			byEmoji[r.Emoji] = a
			order = append(order, a)
		}
		a.count++
		if r.UserID == viewerID {
			a.me = true
		}
		if r.CreatedAt.Before(a.firstTime) {
			a.firstTime = r.CreatedAt
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].firstTime.Before(order[j].firstTime) })
	out := make([]ReactionSummary, len(order))
	for i, a := range order {
		out[i] = ReactionSummary{Emoji: a.emoji, Count: a.count, Me: a.me}
	}
	return out
}

func (r *repo) AddReaction(ctx context.Context, roomID, messageID, userID int64, emoji string) error {
	stmt, names := reactionsTable.Insert()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.BindStruct(&Reaction{
		RoomID:    roomID,
		MessageID: messageID,
		Emoji:     emoji,
		UserID:    userID,
		CreatedAt: time.Now().UTC(),
	}).ExecRelease()
}

func (r *repo) RemoveReaction(ctx context.Context, roomID, messageID, userID int64, emoji string) error {
	stmt, names := reactionsTable.Delete()
	q := r.session.Query(stmt, names).WithContext(ctx)
	return q.Bind(roomID, messageID, emoji, userID).ExecRelease()
}

// ListReactions returns every reaction on one message - a single-partition read.
func (r *repo) ListReactions(ctx context.Context, roomID, messageID int64) ([]Reaction, error) {
	stmt, names := reactionsTable.Select()
	q := r.session.Query(stmt, names).WithContext(ctx)
	defer q.Release()
	var out []Reaction
	if err := q.Bind(roomID, messageID).SelectRelease(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListReactionsForMessages batches the reads for a whole page of history into one query
// instead of one partition read per message.
func (r *repo) ListReactionsForMessages(ctx context.Context, roomID int64, messageIDs []int64) (map[int64][]Reaction, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	cols := strings.Join(reactionsTable.Metadata().Columns, ", ")
	stmt := "SELECT " + cols + " FROM message_reactions WHERE room_id = ? AND message_id IN ?"
	q := r.session.Query(stmt, nil).WithContext(ctx).Bind(roomID, messageIDs)
	defer q.Release()
	iter := q.Iter()
	defer iter.Close()
	out := make(map[int64][]Reaction)
	var row Reaction
	for iter.StructScan(&row) {
		out[row.MessageID] = append(out[row.MessageID], row)
	}
	return out, iter.Close()
}
