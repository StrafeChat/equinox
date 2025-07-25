package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

type MessageAttachment struct {
	Name   string `db:"name" json:"name"`
	URL    string `db:"url" json:"url"`
	Type   string `db:"type" json:"type"`
	Height int    `db:"height" json:"height"`
	Width  int    `db:"width" json:"width"`
	Size   int64  `db:"size" json:"size"`
}

func (ma MessageAttachment) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"name":   ma.Name,
		"url":    ma.URL,
		"type":   ma.Type,
		"height": ma.Height,
		"width":  ma.Width,
		"size":   ma.Size,
	}
}

type MessageEmbedAuthor struct {
	Name    string  `db:"name" json:"name"`
	URL     *string `db:"url" json:"url"`
	IconURL *string `db:"icon_url" json:"icon_url"`
}

func (mea MessageEmbedAuthor) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"name":    mea.Name,
		"url":     mea.URL,
		"iconURL": mea.IconURL,
	}
}

type MessageEmbedFooter struct {
	Text    string  `db:"text" json:"text"`
	IconURL *string `db:"icon_url" json:"icon_url"`
}

func (mef MessageEmbedFooter) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"text":    mef.Text,
		"iconURL": mef.IconURL,
	}
}

type MessageEmbedMedia struct {
	URL    string `db:"url" json:"url"`
	Height int    `db:"height" json:"height"`
	Width  int    `db:"width" json:"width"`
}

func (mem MessageEmbedMedia) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"url":    mem.URL,
		"height": mem.Height,
		"width":  mem.Width,
	}
}

type MessageEmbedField struct {
	Name   string `db:"name" json:"name"`
	Value  string `db:"value" json:"value"`
	Inline bool   `db:"inline" json:"inline"`
}

func (mef MessageEmbedField) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"name":   mef.Name,
		"value":  mef.Value,
		"inline": mef.Inline,
	}
}

type MessageEmbed struct {
	Title       *string                   `db:"title" json:"title"`
	Description *string                   `db:"description" json:"description"`
	URL         *string                   `db:"url" json:"url"`
	Color       *int                      `db:"color" json:"color"`
	Timestamp   *time.Time                `db:"timestamp" json:"timestamp"`
	Author      *map[string]interface{}   `db:"author" json:"author"`
	Footer      *map[string]interface{}   `db:"footer" json:"footer"`
	Image       *map[string]interface{}   `db:"image" json:"image"`
	Thumbnail   *map[string]interface{}   `db:"thumbnail" json:"thumbnail"`
	Video       *map[string]interface{}   `db:"video" json:"video"`
	Fields      *[]map[string]interface{} `db:"fields" json:"fields"`
}

func (me MessageEmbed) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"title":       me.Title,
		"description": me.Description,
		"url":         me.URL,
		"color":       me.Color,
		"timestamp":   me.Timestamp,
		"author":      me.Author,
		"footer":      me.Footer,
		"image":       me.Image,
		"thumbnail":   me.Thumbnail,
		"video":       me.Video,
		"fields":      me.Fields,
	}
}

type MessageSudo struct {
	Name      string  `db:"name" json:"name"`
	AvatarURL string  `db:"avatar_url" json:"avatar_url"`
	Color     *string `db:"color" json:"color"`
}

type MessageSystemData struct {
	Type      string  `db:"type" json:"type"`
	UserID    *string `db:"user_id" json:"user_id,omitempty"`       // For member events
	ActorID   *string `db:"actor_id" json:"actor_id,omitempty"`     // Who performed the action
	OldValue  *string `db:"old_value" json:"old_value,omitempty"`   // For property changes
	NewValue  *string `db:"new_value" json:"new_value,omitempty"`   // For property changes
	ExtraData string  `db:"extra_data" json:"extra_data,omitempty"` // Additional data as JSON string
}

func (ms MessageSudo) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"name":      ms.Name,
		"avatarURL": ms.AvatarURL,
		"color":     ms.Color,
	}
}

func (msd MessageSystemData) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"type":       msd.Type,
		"user_id":    msd.UserID,
		"actor_id":   msd.ActorID,
		"old_value":  msd.OldValue,
		"new_value":  msd.NewValue,
		"extra_data": msd.ExtraData,
	}
}

type Message struct {
	ID                string                    `db:"id" json:"id"`
	Nonce             *string                   `db:"nonce" json:"nonce"`
	RoomID            string                    `db:"room_id" json:"room_id"`
	SpaceID           *int64                    `db:"space_id" json:"space_id,string"`
	AuthorID          *string                   `db:"author_id" json:"author_id"`
	Content           *string                   `db:"content" json:"content"`
	Type              *int                      `db:"type" json:"type"`
	SystemType        *string                   `db:"system_type" json:"system_type"`
	SystemData        *map[string]interface{}   `db:"system_data" json:"system_data"`
	System            bool                      `db:"system" json:"system"`
	TTS               bool                      `db:"tts" json:"tts"`
	Attachments       []*map[string]interface{} `db:"attachments" json:"attachments"`
	Embeds            []*map[string]interface{} `db:"embeds" json:"embeds"`
	Flags             *int                      `db:"flags" json:"flags"`
	MentionEveryone   bool                      `db:"mention_everyone" json:"mention_everyone"`
	MentionRoles      *[]string                 `db:"mention_roles" json:"mention_roles"`
	MentionRooms      *[]string                 `db:"mention_rooms" json:"mention_rooms"`
	Mentions          *[]string                 `db:"mentions" json:"mentions"`
	MessageReferences *[]string                 `db:"message_references" json:"message_references"`
	Pinned            *bool                     `db:"pinned" json:"pinned"`
	CreatedAt         time.Time                 `db:"created_at" json:"created_at"`
	EditedAt          *time.Time                `db:"edited_at" json:"edited_at"`
}

var MessageMeta = table.Metadata{
	Name:    "messages",
	Columns: []string{"id", "nonce", "room_id", "space_id", "author_id", "content", "type", "system_type", "system_data", "system", "tts", "attachments", "embeds", "flags", "mention_everyone", "mention_roles", "mention_rooms", "mentions", "message_references", "pinned", "created_at", "edited_at"},
	PartKey: []string{"id"},
	SortKey: []string{"created_at"},
}

var MessageTable = table.New(MessageMeta)

func (m *Message) SchemaDefinition() []string {
	return []string{
		`CREATE TYPE IF NOT EXISTS message_attachment (
		name text,
		url text,
		type text,
		height int,
		width int,
		size bigint
	);`,
		`CREATE TYPE IF NOT EXISTS message_embed_author (
		name text,
		url text,
		icon_url text
	);`,
		`CREATE TYPE IF NOT EXISTS message_embed_footer (
		text text,
		icon_url text
	);`,
		`CREATE TYPE IF NOT EXISTS message_embed_media (
		url text,
		height int,
		width int
	);`,
		`CREATE TYPE IF NOT EXISTS message_embed_field (
		name text,
		value text,
		inline boolean
	);`,
		`CREATE TYPE IF NOT EXISTS message_embed (
		title text,
		description text,
		url text,
		color int,
		timestamp timestamp,
		author FROZEN<message_embed_author>,
		footer FROZEN<message_embed_footer>,
		image FROZEN<message_embed_media>,
		thumbnail FROZEN<message_embed_media>,
		video FROZEN<message_embed_media>,
		fields SET<FROZEN<message_embed_field>>
	);`,
		`CREATE TYPE IF NOT EXISTS message_sudo (
		name text,
		avatar_url text,
		color text
	);`,
		`CREATE TYPE IF NOT EXISTS message_system_data (
		type text,
		user_id text,
		actor_id text,
		old_value text,
		new_value text,
		extra_data text
	);`,
		`CREATE TABLE IF NOT EXISTS messages (
		id bigint,
		nonce text,
		room_id bigint,
		space_id bigint,
		author_id bigint,
		content text,
		type int,
		system_type text,
		system_data FROZEN<message_system_data>,
		system boolean,
		tts boolean,
		attachments SET<FROZEN<message_attachment>>,
		embeds SET<FROZEN<message_embed>>,
		flags int,
		mention_everyone boolean,
		mention_roles SET<bigint>,
		mention_rooms SET<bigint>,
		mentions SET<bigint>,
		message_references SET<bigint>,
		pinned boolean,
		created_at timestamp,
		edited_at timestamp,
		PRIMARY KEY (id, created_at)
	) WITH CLUSTERING ORDER BY (created_at DESC);`}
}
