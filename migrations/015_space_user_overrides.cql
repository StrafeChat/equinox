CREATE TABLE IF NOT EXISTS space_room_user_overrides (
    space_id bigint,
    room_id bigint,
    user_id bigint,
    allow_mask bigint,
    deny_mask bigint,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY ((space_id, room_id), user_id)
);
