-- The instance's official account ("system"): one per keyspace, used to deliver official
-- messages (moderation outcomes, admin notices) as a direct message to a user. Its id is
-- claimed here, once, so every API node agrees on which users row speaks for the instance.
-- The account itself is an ordinary users row flagged system=true with no password, so it
-- can never log in; it only ever sends.
CREATE TABLE IF NOT EXISTS instance_system_account (
    key text PRIMARY KEY,
    user_id bigint,
    created_at timestamp
);
