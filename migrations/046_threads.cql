-- Threads, Discord's model: a room of type 6 that lives inside a space text channel. It has
-- its own members (joined explicitly, or by posting or being mentioned), an archive state
-- with an auto-archive timer, and a lock. A public thread is visible to whoever can view the
-- parent channel; a private one only to its members and Manage Threads holders. Threads
-- inherit the parent channel's permission overrides (resolved in the space snapshot).
ALTER TABLE rooms ADD thread_archived boolean;
ALTER TABLE rooms ADD thread_archived_at timestamp;
ALTER TABLE rooms ADD thread_auto_archive_minutes int;
ALTER TABLE rooms ADD thread_locked boolean;
ALTER TABLE rooms ADD thread_private boolean;
ALTER TABLE rooms ADD thread_invitable boolean;
ALTER TABLE rooms ADD thread_last_active_at timestamp;
ALTER TABLE rooms ADD thread_starter_message_id bigint;

-- The message a thread was started from points at it (the thread's id is the message's id,
-- as on Discord; stored explicitly so a history page carries it without a lookup each).
ALTER TABLE messages ADD thread_id bigint;

CREATE TABLE IF NOT EXISTS thread_members (
    room_id bigint,
    user_id bigint,
    joined_at timestamp,
    PRIMARY KEY (room_id, user_id)
);

CREATE TABLE IF NOT EXISTS thread_members_by_user (
    user_id bigint,
    room_id bigint,
    joined_at timestamp,
    PRIMARY KEY (user_id, room_id)
);

-- Discord's message_count: goes up on every message in the thread, down on deletes.
CREATE TABLE IF NOT EXISTS thread_counters (
    room_id bigint PRIMARY KEY,
    message_count counter
);

-- Auto-archive queue: one row per (re)scheduling, in hour buckets the worker scans. A row
-- whose due time no longer matches the thread's last activity plus its auto-archive
-- duration is dropped unused. Rows expire on their own after 30 days.
CREATE TABLE IF NOT EXISTS thread_archive_queue (
    bucket text,
    due_at timestamp,
    room_id bigint,
    PRIMARY KEY (bucket, due_at, room_id)
) WITH default_time_to_live = 2592000;
