-- Rooms (channels): polymorphic container for PM, Space channels, threads.

-- Main rooms table: partition by id.
CREATE TABLE IF NOT EXISTS rooms (
    id bigint PRIMARY KEY,
    type int,              -- RoomType: 1=PM, 2=GROUP_PM, 3=SPACE_TEXT, 4=SPACE_VOICE, 5=SPACE_CATEGORY, 6=THREAD
    space_id bigint,       -- null for PMs
    parent_id bigint,      -- null; for threads: parent channel id
    name text,
    topic text,
    position int,
    last_message_id bigint,
    created_at timestamp,
    updated_at timestamp
);

-- Participants for PM/GROUP_PM: who is in the room.
CREATE TABLE IF NOT EXISTS room_participants (
    room_id bigint,
    user_id bigint,
    joined_at timestamp,
    PRIMARY KEY (room_id, user_id)
);

-- Rooms by user: list rooms (PMs) for a user. Sorted by last_message_id DESC for recency.
CREATE TABLE IF NOT EXISTS rooms_by_user (
    user_id bigint,
    room_id bigint,
    last_message_id bigint,
    joined_at timestamp,
    PRIMARY KEY (user_id, room_id)
);

-- 1:1 PM lookup: (smaller_id, larger_id) -> room_id for O(1) find-or-create.
CREATE TABLE IF NOT EXISTS pm_rooms (
    user_a_id bigint,
    user_b_id bigint,
    room_id bigint,
    created_at timestamp,
    PRIMARY KEY (user_a_id, user_b_id)
);

-- Rooms in a space: for future SPACE_TEXT, SPACE_VOICE, SPACE_CATEGORY.
CREATE TABLE IF NOT EXISTS rooms_by_space (
    space_id bigint,
    room_id bigint,
    position int,
    created_at timestamp,
    PRIMARY KEY (space_id, room_id)
);
