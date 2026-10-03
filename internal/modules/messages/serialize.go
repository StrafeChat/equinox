package messages

import (
	"context"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/modules/auth"
)

// Embedding users in message payloads (Discord's model).
//
// A message carries only `sender_id` / `mentions` (ids). The client resolves those against its
// cached member list, which fails for anyone it never cached - someone who left, a bot, an
// uncached member in a large space - and renders "Unknown" with no avatar. Discord avoids this
// by embedding the author (and mentioned users) as full objects on the message itself.
//
// So the serializers here *add* `author`, `mention_users` and, for a reply, `referenced_message`
// alongside the existing id fields (which stay, so nothing already reading them breaks). The
// embedded user object uses the exact same keys the client already parses for room participants
// and space members, and is federation-aware - a shadow row's home_domain/origin_id come through,
// so a remote sender resolves too. Everything is best-effort: a lookup miss simply omits the
// embed and the client falls back to its id-based resolution, so this can never regress a
// message that would otherwise have rendered.

// publicUserJSON is the embedded user object - mirrors rooms.participantOf / the space member
// list shape so the client's existing participant parsing handles it unchanged.
func (s *Service) publicUserJSON(u *auth.User) map[string]interface{} {
	m := map[string]interface{}{
		"id":            id.Format(u.ID),
		"username":      u.Username,
		"discriminator": u.Discriminator,
		"display_name":  u.DisplayName,
		"avatar":        u.Avatar,
		"banner":        u.Banner,
		"bio":           u.Bio,
		"about_me":      u.AboutMe,
		"bot":           u.Bot,
		"public_flags":  auth.PublicFlags(u),
		"presence":      auth.ToPublicPresence(u.Presence, true),
	}
	if local := s.localDomain(); local != "" {
		m["home_domain"] = local
		if u.IsRemote() {
			m["home_domain"] = u.HomeDomain
		}
		m["origin_id"] = id.Format(u.OriginID())
	}
	return m
}

func (s *Service) localDomain() string {
	if s.cfg == nil {
		return ""
	}
	return s.cfg.Federation.Domain
}

// userObjects batch-fetches the embedded user objects for a set of ids (deduped), one
// cache-backed GetByIDs, returning id -> object. Shadow rows resolve here too, so federated
// senders get embedded like local ones.
func (s *Service) userObjects(ctx context.Context, ids []int64) map[int64]map[string]interface{} {
	out := make(map[int64]map[string]interface{})
	if s.userRepo == nil || len(ids) == 0 {
		return out
	}
	seen := make(map[int64]struct{}, len(ids))
	uniq := make([]int64, 0, len(ids))
	for _, uid := range ids {
		if uid == 0 {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		uniq = append(uniq, uid)
	}
	if len(uniq) == 0 {
		return out
	}
	users, err := s.userRepo.GetByIDs(ctx, uniq)
	if err != nil {
		return out
	}
	for _, u := range users {
		if u != nil {
			out[u.ID] = s.publicUserJSON(u)
		}
	}
	return out
}

// embedUsers layers `author` and `mention_users` onto a base message map from a prefetched pool.
func embedUsers(out map[string]interface{}, m *Message, pool map[int64]map[string]interface{}) {
	if a, ok := pool[m.SenderID]; ok {
		out["author"] = a
	}
	if len(m.Mentions) > 0 {
		mu := make([]map[string]interface{}, 0, len(m.Mentions))
		for _, mid := range m.Mentions {
			if a, ok := pool[mid]; ok {
				mu = append(mu, a)
			}
		}
		if len(mu) > 0 {
			out["mention_users"] = mu
		}
	}
}

// referencedMessage fetches the (non-deleted) message a reply points at, within the same room.
func (s *Service) referencedMessage(ctx context.Context, m *Message) *Message {
	if m.ReplyToID == nil {
		return nil
	}
	ref, err := s.repo.GetByID(ctx, m.RoomID, *m.ReplyToID)
	if err != nil || ref == nil || (ref.DeletedAt != nil && !ref.DeletedAt.IsZero()) {
		return nil
	}
	return ref
}

// referencedMessageJSON builds the embedded `referenced_message` (one level deep - the quoted
// message never carries its own referenced_message), with its own author embedded.
func (s *Service) referencedMessageJSON(ctx context.Context, m *Message) map[string]interface{} {
	ref := s.referencedMessage(ctx, m)
	if ref == nil {
		return nil
	}
	rj := map[string]interface{}(messageToJSON(ref, nil))
	embedUsers(rj, ref, s.userObjects(ctx, append([]int64{ref.SenderID}, ref.Mentions...)))
	return rj
}

// MessageJSON serializes one message with author / mentioned users / referenced message embedded.
func (s *Service) MessageJSON(ctx context.Context, m *Message, reactions []ReactionSummary) map[string]interface{} {
	out := map[string]interface{}(messageToJSON(m, reactions))
	embedUsers(out, m, s.userObjects(ctx, append([]int64{m.SenderID}, m.Mentions...)))
	if ref := s.referencedMessageJSON(ctx, m); ref != nil {
		out["referenced_message"] = ref
	}
	return out
}

// MessagesJSON serializes a page of messages, batching the author/mention lookup into a single
// GetByIDs (referenced messages' authors folded in), so a history page is a couple of lookups
// rather than one per row.
func (s *Service) MessagesJSON(ctx context.Context, msgs []Message, reactionsByMsg map[int64][]ReactionSummary) []map[string]interface{} {
	ids := make([]int64, 0, len(msgs)*2)
	for i := range msgs {
		ids = append(ids, msgs[i].SenderID)
		ids = append(ids, msgs[i].Mentions...)
	}
	// Resolve each distinct reply target once, and fold their senders into the same id batch.
	refs := make(map[int64]*Message)
	for i := range msgs {
		if msgs[i].ReplyToID == nil {
			continue
		}
		rid := *msgs[i].ReplyToID
		if _, done := refs[rid]; done {
			continue
		}
		if ref := s.referencedMessage(ctx, &msgs[i]); ref != nil {
			refs[rid] = ref
			ids = append(ids, ref.SenderID)
			ids = append(ids, ref.Mentions...)
		} else {
			refs[rid] = nil
		}
	}
	pool := s.userObjects(ctx, ids)
	out := make([]map[string]interface{}, len(msgs))
	for i := range msgs {
		row := map[string]interface{}(messageToJSON(&msgs[i], reactionsByMsg[msgs[i].ID]))
		embedUsers(row, &msgs[i], pool)
		if msgs[i].ReplyToID != nil {
			if ref := refs[*msgs[i].ReplyToID]; ref != nil {
				rj := map[string]interface{}(messageToJSON(ref, nil))
				embedUsers(rj, ref, pool)
				row["referenced_message"] = rj
			}
		}
		out[i] = row
	}
	return out
}

// messageEventPayloadEnriched is messageEventPayload plus the embedded author / mention users /
// referenced message, for the realtime MESSAGE_CREATE / MESSAGE_UPDATE frames.
func (s *Service) messageEventPayloadEnriched(ctx context.Context, m *Message) map[string]interface{} {
	out := messageEventPayload(m)
	embedUsers(out, m, s.userObjects(ctx, append([]int64{m.SenderID}, m.Mentions...)))
	if ref := s.referencedMessageJSON(ctx, m); ref != nil {
		out["referenced_message"] = ref
	}
	return out
}
