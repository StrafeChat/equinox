-- Instance invites: codes that admit a *new account* to this instance when INVITE_ONLY is
-- on. Distinct from space_invites, which admit an existing account to a space, and kept in
-- their own tables because nothing about them belongs to a space.
CREATE TABLE IF NOT EXISTS instance_invites (
    code text PRIMARY KEY,
    created_by bigint,
    note text,
    max_uses int,
    uses int,
    expires_at timestamp,
    created_at timestamp
);

-- Listing every live code without a range scan. One partition is correct here: an
-- instance's open invites are a short list an operator reads occasionally, not a hot path.
-- Every column is repeated here rather than storing codes and fetching each one, so the
-- listing is a single partition read.
CREATE TABLE IF NOT EXISTS instance_invites_by_bucket (
    bucket int,
    code text,
    created_by bigint,
    note text,
    max_uses int,
    uses int,
    expires_at timestamp,
    created_at timestamp,
    PRIMARY KEY (bucket, code)
);

-- Single-row table recording that this instance has had its first account. The first
-- registration claims it with a lightweight transaction, which is what lets an operator
-- create their own account on a brand-new invite-only instance without an invite existing
-- yet - and stops anyone else racing them for it.
CREATE TABLE IF NOT EXISTS instance_state (
    key text PRIMARY KEY,
    bootstrapped_by bigint,
    bootstrapped_at timestamp
);

-- Who may mint and revoke instance invites. Set on the account that claimed the bootstrap
-- above; INSTANCE_ADMINS in the environment is the other way in, so an operator who loses
-- that account is not locked out.
ALTER TABLE users ADD instance_admin boolean;
