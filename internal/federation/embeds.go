package federation

import (
	"context"
	"strings"

	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/messages"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
)

// A bot's rich embeds (messages/embeds.go) cross instances in their stored form, on every
// message body that carries content (MessageEvent, MessageEdit, SpaceMessageRequest,
// SpaceMessageEditRequest): no type and no proxy URLs - each instance serves the images
// through its own proxy, so no reader's address reaches the image's host - but with the image
// sizes the origin learned. Mentions in a description or a field value are written with local
// ids like message text and are translated at the boundary the same way (content.go); the
// custom emoji anywhere in their text are described in the body's emojis with the text's.
//
// What a peer sends is held to the rules a local send is: the same validator, never an
// attachment:// (a peer sends only resolved URLs), none at all in an end-to-end encrypted
// room, and only on a bot's message. The limits count the text as it is stored here, with
// mentions translated (localEmbeds): on the wire a mention is longer. An instance from
// before embeds sends none and ignores the field, which leaves it exactly as it was.

// embedsToWire is a message's embeds for the wire, mentions as <@FID>.
func (s *Service) embedsToWire(ctx context.Context, list []messages.Embed) []messages.Embed {
	return mapEmbedText(list, func(text string) string { return s.textToWire(ctx, text) })
}

// embedsFromWire rewrites the <@FID> mentions in embeds as local ids, with one lookup budget
// for the whole list (the message's text has its own).
func (s *Service) embedsFromWire(ctx context.Context, list []messages.Embed) []messages.Embed {
	budget := maxMentionLookups
	return mapEmbedText(list, func(text string) string { return s.textFromWireWithin(ctx, text, &budget) })
}

// mapEmbedText copies list with f applied to the text in which a client renders mentions:
// descriptions and field values. Titles and field names are inline markdown only, and author
// and footer lines plain text. nil for none.
func mapEmbedText(list []messages.Embed, f func(string) string) []messages.Embed {
	if len(list) == 0 {
		return nil
	}
	out := make([]messages.Embed, len(list))
	for i, e := range list {
		e.Description = f(e.Description)
		if len(e.Fields) > 0 {
			fields := make([]messages.EmbedField, len(e.Fields))
			for j, field := range e.Fields {
				field.Value = f(field.Value)
				fields[j] = field
			}
			e.Fields = fields
		}
		out[i] = e
	}
	return out
}

// withEmbedText is a message's text followed by its embeds', for emojiRefs: an emoji used
// only in an embed has to render on the receiving instance too.
func withEmbedText(text string, list []messages.Embed) string {
	if len(list) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	add := func(s string) {
		if s != "" {
			b.WriteByte('\n')
			b.WriteString(s)
		}
	}
	for _, e := range list {
		add(e.Title)
		add(e.Description)
		for _, f := range e.Fields {
			add(f.Name)
			add(f.Value)
		}
	}
	return b.String()
}

// maxMentionShrink is the most translating one mention can shorten a text by: a resolvable
// <@!@id:domain:port> at its longest (an int64 id, a 253-character domain, a port) against
// <@id> at its shortest.
const maxMentionShrink = len("<@!@:>") + 19 + 253 + len(":65535") - len("<@0>")

// localEmbeds is a peer's embeds as this instance keeps them, held to the local validator.
// The mentions are translated first, under the message's lookup budget, so the limits count
// the text as it is stored and served here - the sender's instance accepted it in that form,
// and on the wire each mention is longer. Before that, a cheap look at the wire form refuses
// what could not fit however the translation came out, so a peer cannot make this instance
// translate (or fetch profiles for) text it will refuse anyway.
func (s *Service) localEmbeds(ctx context.Context, list []messages.Embed) ([]messages.Embed, error) {
	if err := messages.CheckEmbedsWithin(list, maxMentionLookups*maxMentionShrink); err != nil {
		return nil, err
	}
	return messages.NormalizeEmbeds(s.embedsFromWire(ctx, list), nil)
}

// acceptEmbeds is what this instance keeps of the embeds a peer sent on a message by sender
// in room: none in an end-to-end encrypted room or on a message with ciphertext, whatever the
// peer says, and none on a message that is not a bot's (a system notice included), so that a
// person's message never looks like a bot's card here. Otherwise it is the list as the local
// validator leaves it, mentions translated. A list the validator refuses is dropped whole and
// the rest of the message kept: the origin has already accepted it, and losing the text over
// its embeds would help nobody. A mirror's own write to the origin is judged instead
// (peerEmbeds).
func (s *Service) acceptEmbeds(ctx context.Context, room *rooms.Room, sender *auth.User, ciphertext string, list []messages.Embed) []messages.Embed {
	if len(list) == 0 || ciphertext != "" || sender == nil || !sender.Bot || !messages.EmbedsAllowedIn(room) {
		return nil
	}
	norm, err := s.localEmbeds(ctx, list)
	if err != nil {
		logger.Warn("federation", "dropping the embeds of a relayed message in room %d: %v", room.ID, err)
		return nil
	}
	return norm
}

// peerEmbeds is the embeds of a write a mirror forwards for its member, held to the local
// validator before anything else reads them - a refusal reaches the member as a local
// sender's would - with the mentions translated. Who may send embeds, and where, is the
// messages service's to judge, as for a local send (the bot flag of the member's shadow,
// Embed Links, the room's encryption).
func (s *Service) peerEmbeds(ctx context.Context, list []messages.Embed) ([]messages.Embed, error) {
	return s.localEmbeds(ctx, list)
}
