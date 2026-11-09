package federation

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Message text is written with instance-local ids: a mention is <@localUserId>, a custom
// emoji <:name:emojiId>. Neither means anything on another instance as-is - the same
// person has a different shadow id everywhere, and an emoji id alone cannot say which
// instance's CDN holds the image - so text is translated at the boundary:
//
//   - mentions go out as <@FID> (textToWire) and come back in as <@localId> of the
//     receiver's shadow row (textFromWire). A person the receiver has never heard of is
//     fetched from their home instance so the pill can render; failing that the token is
//     left as it came, which the client shows as "unknown" - no worse than before.
//   - custom emoji keep their ids (they are unique enough across instances), and every
//     event that carries text or a reaction also carries EmojiRefs describing the emoji it
//     uses (name, image URL, animated). The receiver records the ones it does not know so
//     GET /emojis/:id can resolve them; an emoji it already knows is never overwritten.
//
// Role mentions (<@&id>) keep the origin's role ids everywhere already.

var (
	// <@123> or <@!123>: a local user id in text.
	wireUserMentionRe = regexp.MustCompile(`<@!?(\d+)>`)
	// <@@123:domain>: a federated id in text, the wire form.
	wireFIDMentionRe = regexp.MustCompile(`<@!?(@\d+:[A-Za-z0-9.\-]+(?::\d+)?)>`)
	// <:name:123> / <a:name:123>: a custom emoji (Discord's syntax, what the client parses).
	wireCustomEmojiRe = regexp.MustCompile(`<a?:[A-Za-z0-9_]{2,32}:(\d+)>`)
)

// maxEmojiRefs bounds how many emoji one event may describe (and so how many rows a
// peer can make us write per message).
const maxEmojiRefs = 50

// textToWire rewrites <@localId> mentions as <@FID> for the wire.
func (s *Service) textToWire(ctx context.Context, text string) string {
	if text == "" || !strings.Contains(text, "<@") {
		return text
	}
	return wireUserMentionRe.ReplaceAllStringFunc(text, func(tok string) string {
		m := wireUserMentionRe.FindStringSubmatch(tok)
		uid, err := id.Parse(m[1])
		if err != nil {
			return tok
		}
		u, err := s.users.GetByID(ctx, uid)
		if err != nil || u == nil {
			return tok
		}
		return "<@" + s.FIDOf(u) + ">"
	})
}

// maxMentionLookups bounds the mentions textFromWire resolves in one text. Each unknown one
// may cost a signed profile fetch from a third instance; a message gets a handful, and the
// rest stay as they came (the client renders them as unknown).
const maxMentionLookups = 20

// textFromWire rewrites <@FID> mentions as <@localId> of the shadow (or local user) the
// FID names here, fetching a shadow for anyone unknown.
func (s *Service) textFromWire(ctx context.Context, text string) string {
	budget := maxMentionLookups
	return s.textFromWireWithin(ctx, text, &budget)
}

// textFromWireWithin is textFromWire spending its lookups from budget, which several texts of
// one message share (its embeds' descriptions and field values, embeds.go).
func (s *Service) textFromWireWithin(ctx context.Context, text string, budget *int) string {
	if text == "" || !strings.Contains(text, "<@@") {
		return text
	}
	return wireFIDMentionRe.ReplaceAllStringFunc(text, func(tok string) string {
		if *budget <= 0 {
			return tok
		}
		*budget--
		m := wireFIDMentionRe.FindStringSubmatch(tok)
		u, err := s.shadowByFID(ctx, m[1])
		if err != nil || u == nil {
			return tok
		}
		return "<@" + id.Format(u.ID) + ">"
	})
}

// A system message's payload is JSON naming people and rooms by instance-local ids, so it is
// translated at the boundary as text is: every user goes out as an FID and every room as a room
// FID, and both come in as this instance's ids. message_id stays, because a channel keeps the
// origin's message ids, and so does everything else in the payload. An older origin sends plain
// numbers, which have no meaning here and stay as they came: its notices read "Someone", as
// they always have, and an older mirror keeps these FIDs the same way.
var systemPayloadIDKeys = []string{"actor_id", "user_id", "thread_id"}

// rewriteSystemPayload applies rewrite to the id fields of a payload and leaves the rest of it
// exactly as it was. A payload that is not a JSON object, or holds no ids, comes back unchanged.
func rewriteSystemPayload(raw string, rewrite func(key, value string) string) string {
	if raw == "" {
		return raw
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return raw
	}
	changed := false
	for _, key := range systemPayloadIDKeys {
		rawValue, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(rawValue, &value) != nil {
			continue
		}
		next := rewrite(key, value)
		if next == value {
			continue
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			continue
		}
		fields[key] = encoded
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return string(out)
}

// systemPayloadToWire rewrites a stored payload's local ids as FIDs.
func (s *Service) systemPayloadToWire(ctx context.Context, raw string) string {
	return rewriteSystemPayload(raw, func(key, value string) string {
		local, err := id.Parse(value)
		if err != nil {
			return value
		}
		if key == "thread_id" {
			// A thread lives on its space's origin, and only an origin relays one.
			return RoomFID(local, s.fcfg.Domain)
		}
		u, err := s.users.GetByID(ctx, local)
		if err != nil || u == nil {
			return value
		}
		return s.FIDOf(u)
	})
}

// systemPayloadFromWire rewrites a relayed payload's FIDs as this instance's ids, fetching a
// shadow for anyone unknown. What cannot be resolved stays as it came, which a client shows
// as "Someone" - no worse than before.
func (s *Service) systemPayloadFromWire(ctx context.Context, raw string) string {
	return rewriteSystemPayload(raw, func(key, value string) string {
		if key == "thread_id" {
			originRoomID, domain, err := ParseRoomFID(value)
			if err != nil {
				return value
			}
			local, err := s.localRoomID(ctx, domain, originRoomID, false)
			if err != nil {
				return value
			}
			return id.Format(local)
		}
		if !strings.HasPrefix(value, "@") {
			return value
		}
		u, err := s.shadowByFID(ctx, value)
		if err != nil || u == nil {
			return value
		}
		return id.Format(u.ID)
	})
}

// emojiRefs describes the custom emoji referenced in text (plus any extra reaction emoji
// given as "custom:<id>") that this instance knows, for a peer that may not.
func (s *Service) emojiRefs(ctx context.Context, text string, reactions ...string) []EmojiRef {
	if s.spaceSvc == nil {
		return nil
	}
	seen := map[int64]struct{}{}
	var out []EmojiRef
	add := func(raw string) {
		if len(out) >= maxEmojiRefs {
			return
		}
		eid, err := id.Parse(raw)
		if err != nil {
			return
		}
		if _, dup := seen[eid]; dup {
			return
		}
		seen[eid] = struct{}{}
		ref, err := s.spaceSvc.LookupEmoji(ctx, eid)
		if err != nil || ref == nil {
			return
		}
		out = append(out, EmojiRef{ID: id.Format(ref.ID), Name: ref.Name, URL: ref.URL, Animated: ref.Animated})
	}
	if strings.Contains(text, "<:") || strings.Contains(text, "<a:") {
		for _, m := range wireCustomEmojiRe.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	for _, r := range reactions {
		if strings.HasPrefix(r, "custom:") {
			add(strings.TrimPrefix(r, "custom:"))
		}
	}
	return out
}

// emojiRefFor is emojiRefs for one reaction emoji: the ref, or nil for a Unicode one.
func (s *Service) emojiRefFor(ctx context.Context, emoji string) *EmojiRef {
	refs := s.emojiRefs(ctx, "", emoji)
	if len(refs) == 0 {
		return nil
	}
	return &refs[0]
}

// recordEmojiRefs stores the emoji a peer described so they render here. Only well-formed
// refs with an http(s) image are kept, and an id this instance already knows is left
// alone (spaces.RecordForeignEmoji) - a peer can add to what we can show, not change it.
func (s *Service) recordEmojiRefs(ctx context.Context, refs []EmojiRef) {
	if s.spaceSvc == nil || len(refs) == 0 {
		return
	}
	if len(refs) > maxEmojiRefs {
		refs = refs[:maxEmojiRefs]
	}
	for _, r := range refs {
		eid, err := id.Parse(r.ID)
		if err != nil {
			continue
		}
		name, err := spaces.ValidateEmojiName(r.Name)
		if err != nil {
			continue
		}
		url := strings.TrimSpace(r.URL)
		if len(url) > 512 || (!strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://")) {
			continue
		}
		if err := s.spaceSvc.RecordForeignEmoji(ctx, &spaces.SpaceEmojiRef{ID: eid, Name: name, URL: url, Animated: r.Animated}); err != nil {
			logger.Err("federation", err, map[string]any{"stage": "record_emoji", "emoji": strconv.FormatInt(eid, 10)})
		}
	}
}
