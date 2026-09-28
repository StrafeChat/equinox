-- Message attachments and space custom emoji.
--
-- Attachments: files live on Nebula; equinox keeps a registry row per upload
-- (message_attachments) so a message can only reference uploads its own sender made into
-- the same room, and a denormalised JSON copy on the message row (messages.attachments)
-- so listing a page of history is still a single partition read. In E2EE rooms the
-- registry/JSON copy only carry id/url/size - the file name, type, dimensions and the
-- AES key live inside the Megolm ciphertext.
ALTER TABLE messages ADD attachments text;

CREATE TABLE IF NOT EXISTS message_attachments (
    room_id bigint,
    id bigint,
    uploader_id bigint,
    message_id bigint,             -- set once a message references it; null = orphan upload
    filename text,
    content_type text,
    size bigint,
    url text,
    width int,
    height int,
    encrypted boolean,
    created_at timestamp,
    PRIMARY KEY (room_id, id)
);

-- Custom emoji: space_emoji existed in 013 (unused until now). The image lives on
-- Nebula; space_emoji_by_id lets a client resolve <:name:id> used *outside* its home
-- space (in a PM, or another space) without being a member there.
ALTER TABLE space_emoji ADD url text;
ALTER TABLE space_emoji ADD animated boolean;
ALTER TABLE space_emoji ADD creator_id bigint;

CREATE TABLE IF NOT EXISTS space_emoji_by_id (
    id bigint,
    space_id bigint,
    name text,
    url text,
    animated boolean,
    PRIMARY KEY (id)
);
