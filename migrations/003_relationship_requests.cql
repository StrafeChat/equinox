-- Relationship requests: friend requests sent between users.
-- Dual tables for query by recipient (incoming) and sender (outgoing).

CREATE TABLE IF NOT EXISTS relationship_requests_by_recipient (
    to_user_id bigint,
    from_user_id bigint,
    created_at timestamp,
    PRIMARY KEY (to_user_id, from_user_id)
);

CREATE TABLE IF NOT EXISTS relationship_requests_by_sender (
    from_user_id bigint,
    to_user_id bigint,
    created_at timestamp,
    PRIMARY KEY (from_user_id, to_user_id)
);
