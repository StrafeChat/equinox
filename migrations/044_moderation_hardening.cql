-- Anti-abuse pass for the public beta.

-- Instance IP bans: a CIDR (a single address is stored as /32 or /128) that may not
-- register or sign in. One partition, read whole into memory by every API process and
-- matched per request - an instance has tens of these, not millions. expires_at null
-- means until lifted. Complements instance_bans (accounts): a banned person who makes a
-- new account from the same network is stopped at the door.
CREATE TABLE IF NOT EXISTS instance_ip_bans (
    bucket int,
    cidr text,
    banned_by bigint,
    reason text,
    created_at timestamp,
    expires_at timestamp,
    PRIMARY KEY ((bucket), cidr)
);

-- Per-space light automod, alongside the verification_level column that already existed
-- (and is now enforced on send). automod_flags is a bitfield: 1 block repeated messages,
-- 2 block invite links to other spaces, 4 block messages mentioning more than
-- automod_mention_limit people/roles (0 = the default of 5).
ALTER TABLE spaces ADD automod_flags int;
ALTER TABLE spaces ADD automod_mention_limit int;
