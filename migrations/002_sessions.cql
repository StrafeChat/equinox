-- Session tables (opaque, hashed tokens).
-- Run in the same keyspace as users.

-- Sessions by user: list/revoke sessions for a user.
CREATE TABLE IF NOT EXISTS sessions_by_user (
    user_id bigint,
    session_id bigint,
    token_hash text,
    created_at timestamp,
    expires_at timestamp,
    ip_address text,
    user_agent text,
    device_name text,
    revoked_at timestamp,
    PRIMARY KEY ((user_id), session_id)
) WITH CLUSTERING ORDER BY (session_id DESC);

-- Sessions by token: validate a token quickly.
CREATE TABLE IF NOT EXISTS sessions_by_token (
    token_hash text PRIMARY KEY,
    user_id bigint,
    session_id bigint,
    created_at timestamp,
    expires_at timestamp,
    ip_address text,
    user_agent text,
    device_name text,
    revoked_at timestamp
);

