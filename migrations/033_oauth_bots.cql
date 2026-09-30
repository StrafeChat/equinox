-- Developer platform, round two: bot installs into spaces and a refresh-token index.
--
-- bot_public: whether anyone may add the application's bot to a space they manage, or
-- only the application's owner (Discord's "Public Bot" switch). NULL on rows that predate
-- this column reads as false, i.e. private, which is the safe default for an existing bot
-- nobody has consciously published; new bots are created public.
ALTER TABLE applications ADD bot_public boolean;

-- Refresh-token lookup. The token endpoint used to scan every oauth_tokens row to find
-- the one holding a given refresh hash; this maps the refresh hash straight to the
-- access token's hash. Written with the token, deleted with it.
CREATE TABLE IF NOT EXISTS oauth_tokens_by_refresh (
    refresh_hash text PRIMARY KEY,
    token_hash text
);

-- A role that belongs to a bot (created by its install with the permissions the installer
-- granted): bot_id is the bot's user id. Such a role can be edited but not deleted or
-- given to anyone else, and it goes away when the bot leaves the space - Discord's
-- "managed" role. NULL (every role from before) means an ordinary role.
ALTER TABLE space_roles ADD bot_id bigint;
