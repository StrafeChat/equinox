
CREATE TABLE IF NOT EXISTS space_room_role_overrides (
    space_id bigint,
    room_id bigint,
    role_id bigint,
    allow_mask bigint,
    deny_mask bigint,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY ((space_id, room_id), role_id)
);
