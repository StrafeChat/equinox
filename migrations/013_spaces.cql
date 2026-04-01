-- Spaces and related tables.

CREATE TABLE IF NOT EXISTS spaces (
    id bigint PRIMARY KEY,
    name text,
    name_acronym text,
    description text,
    icon text,
    banner text,
    owner_id bigint,
    verification_level int,
    default_message_notifications int,
    explicit_content_filter int,
    features list<text>,
    afk_room_id bigint,
    afk_timeout int,
    system_room_id bigint,
    system_room_flags int,
    rules_room_id bigint,
    max_presences int,
    max_members int,
    vanity_url_code text,
    preferred_locale text,
    public_updates_room_id bigint,
    max_video_room_users int,
    created_at timestamp,
    updated_at timestamp
);

CREATE TABLE IF NOT EXISTS space_members (
    space_id bigint,
    user_id bigint,
    role_ids set<bigint>,
    nickname text,
    joined_at timestamp,
    PRIMARY KEY (space_id, user_id)
);

CREATE TABLE IF NOT EXISTS spaces_by_user (
    user_id bigint,
    space_id bigint,
    joined_at timestamp,
    PRIMARY KEY (user_id, space_id)
);

CREATE TABLE IF NOT EXISTS space_roles (
    space_id bigint,
    "id" bigint,
    name text,
    permissions bigint,
    position int,
    color int,
    hoist boolean,
    mentionable boolean,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY (space_id, "id")
);

CREATE TABLE IF NOT EXISTS space_invites (
    space_id bigint,
    code text,
    inviter_id bigint,
    max_uses int,
    current_uses int,
    expires_at timestamp,
    created_at timestamp,
    PRIMARY KEY (space_id, code)
);

CREATE TABLE IF NOT EXISTS space_invite_by_code (
    code text PRIMARY KEY,
    space_id bigint,
    inviter_id bigint,
    max_uses int,
    current_uses int,
    expires_at timestamp,
    created_at timestamp
);

CREATE TABLE IF NOT EXISTS space_bans (
    space_id bigint,
    user_id bigint,
    reason text,
    banned_by bigint,
    created_at timestamp,
    PRIMARY KEY (space_id, user_id)
);

CREATE TABLE IF NOT EXISTS space_audit_log (
    space_id bigint,
    "id" bigint,
    action_type text,
    user_id bigint,
    target_id text,
    changes text,
    reason text,
    created_at timestamp,
    PRIMARY KEY (space_id, "id")
) WITH CLUSTERING ORDER BY ("id" DESC);

CREATE TABLE IF NOT EXISTS space_room_overrides (
    space_id bigint,
    room_id bigint,
    "allow" bigint,
    "deny" bigint,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY (space_id, room_id)
);

CREATE TABLE IF NOT EXISTS space_emoji (
    space_id bigint,
    "id" bigint,
    name text,
    image_hash text,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY (space_id, "id")
);
