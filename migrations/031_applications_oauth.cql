-- Developer platform: applications, their bots, and the OAuth2 grant tables.
--
-- An application is an OAuth2 client owned by a user. It may have one bot account (a real
-- user row with bot = true) whose token is what a bot program authenticates with. Third
-- party apps use the OAuth2 authorization-code grant to act for a user within granted scopes.

CREATE TABLE IF NOT EXISTS applications (
    id bigint PRIMARY KEY,
    owner_id bigint,
    name text,
    description text,
    icon text,
    secret_hash text,        -- sha-256 of the client secret; the raw value is shown once
    bot_user_id bigint,      -- 0 until a bot is added
    redirect_uris list<text>,
    created_at timestamp,
    updated_at timestamp
);

-- List an owner's applications without a scan.
CREATE TABLE IF NOT EXISTS applications_by_owner (
    owner_id bigint,
    id bigint,
    created_at timestamp,
    PRIMARY KEY (owner_id, id)
);

-- Resolve a bot token to its bot user. token_hash is sha-256 of the raw token; the raw
-- token is shown once at creation/reset and never stored. One live token per bot: resetting
-- deletes the old row.
CREATE TABLE IF NOT EXISTS bot_tokens (
    token_hash text PRIMARY KEY,
    bot_user_id bigint,
    application_id bigint,
    created_at timestamp
);

-- The bot's current token hash, so a reset can delete the previous bot_tokens row.
CREATE TABLE IF NOT EXISTS bot_token_by_app (
    application_id bigint PRIMARY KEY,
    token_hash text
);

-- Short-lived authorization codes (single use). A reconciler is not needed: each is checked
-- for expiry on redemption and deleted, and unredeemed codes are harmless once expired.
CREATE TABLE IF NOT EXISTS oauth_codes (
    code_hash text PRIMARY KEY,
    user_id bigint,
    application_id bigint,
    scopes list<text>,
    redirect_uri text,
    created_at timestamp
);

-- Issued OAuth2 access tokens. token_hash is sha-256 of the bearer token. Scopes bound the
-- token; expiry is enforced on use. refresh_hash lets a client mint a new access token.
CREATE TABLE IF NOT EXISTS oauth_tokens (
    token_hash text PRIMARY KEY,
    user_id bigint,
    application_id bigint,
    scopes list<text>,
    refresh_hash text,
    expires_at timestamp,
    created_at timestamp
);

-- What a user has authorized, so they can review and revoke. One row per (user, app); a new
-- grant replaces it.
CREATE TABLE IF NOT EXISTS oauth_grants_by_user (
    user_id bigint,
    application_id bigint,
    scopes list<text>,
    token_hash text,
    created_at timestamp,
    PRIMARY KEY (user_id, application_id)
);
