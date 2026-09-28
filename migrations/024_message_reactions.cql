-- Message reactions: unicode or custom-space-emoji reactions on a message. One row per
-- (message, emoji, user) so add/remove is a single-row write, and a message's whole
-- reaction set is one partition read (same shape as message_attachments).
CREATE TABLE IF NOT EXISTS message_reactions (
    room_id bigint,
    message_id bigint,
    emoji text,       -- unicode emoji string, or "custom:<emoji_id>" for a space emoji
    user_id bigint,
    created_at timestamp,
    PRIMARY KEY ((room_id, message_id), emoji, user_id)
);
