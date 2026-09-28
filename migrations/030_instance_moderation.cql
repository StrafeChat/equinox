-- Instance-level moderation: bans that keep an account off this instance entirely (as
-- opposed to space bans, which keep a member out of one space), reports from users about
-- users or spaces, and the audit trail of what an instance administrator did about them.

CREATE TABLE IF NOT EXISTS instance_bans (
    user_id bigint PRIMARY KEY,
    banned_by bigint,
    reason text,
    created_at timestamp,
    expires_at timestamp
);

-- One partition so the bans page is a single read; an instance's bans are a short list.
CREATE TABLE IF NOT EXISTS instance_bans_by_bucket (
    bucket int,
    user_id bigint,
    banned_by bigint,
    reason text,
    created_at timestamp,
    expires_at timestamp,
    PRIMARY KEY (bucket, user_id)
);

-- The report itself, by id.
CREATE TABLE IF NOT EXISTS reports (
    id bigint PRIMARY KEY,
    reporter_id bigint,
    target_type text,
    target_id bigint,
    space_id bigint,
    room_id bigint,
    message_id bigint,
    reason text,
    details text,
    status text,
    created_at timestamp,
    resolved_by bigint,
    resolved_at timestamp,
    resolution text,
    resolution_note text
);

-- The queue: one partition per status, newest first. A status change deletes from one
-- partition and inserts into another; the row in `reports` is the source of truth.
CREATE TABLE IF NOT EXISTS reports_by_status (
    status text,
    id bigint,
    reporter_id bigint,
    target_type text,
    target_id bigint,
    space_id bigint,
    room_id bigint,
    message_id bigint,
    reason text,
    details text,
    created_at timestamp,
    resolved_by bigint,
    resolved_at timestamp,
    resolution text,
    resolution_note text,
    PRIMARY KEY (status, id)
) WITH CLUSTERING ORDER BY (id DESC);

-- Everything ever filed against one user or space, for the detail page and for refusing a
-- second open report from the same person about the same target.
CREATE TABLE IF NOT EXISTS reports_by_target (
    target_type text,
    target_id bigint,
    id bigint,
    reporter_id bigint,
    status text,
    reason text,
    created_at timestamp,
    PRIMARY KEY ((target_type, target_id), id)
) WITH CLUSTERING ORDER BY (id DESC);

-- What administrators did, newest first. Instance-wide rather than per space, because
-- none of these actions belong to a space.
CREATE TABLE IF NOT EXISTS instance_audit_log (
    bucket int,
    id bigint,
    actor_id bigint,
    action text,
    target_type text,
    target_id bigint,
    reason text,
    created_at timestamp,
    PRIMARY KEY (bucket, id)
) WITH CLUSTERING ORDER BY (id DESC);
