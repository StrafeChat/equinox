-- Pinned messages, Discord's model: a room keeps an ordered list of pinned messages that
-- everyone in it sees (until now pins lived only in each client's local storage).
--
-- The flag rides on the message row so every read of a message carries `pinned` for free;
-- room_pins is the room's list itself, newest pin first, which is also how the per-room cap
-- (50, like Discord) is counted. The two are written together in a logged batch.
ALTER TABLE messages ADD pinned_at timestamp;
ALTER TABLE messages ADD pinned_by bigint;

CREATE TABLE IF NOT EXISTS room_pins (
    room_id bigint,
    pinned_at timestamp,
    message_id bigint,
    pinned_by bigint,
    PRIMARY KEY (room_id, pinned_at, message_id)
) WITH CLUSTERING ORDER BY (pinned_at DESC, message_id DESC);
