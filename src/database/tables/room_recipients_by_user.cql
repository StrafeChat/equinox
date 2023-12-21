CREATE TABLE IF NOT EXISTS {keyspace}.room_recipients_by_user (
    room_id TEXT,
    user_id TEXT,
    recipients SET<TEXT>,
    created_at TIMESTAMP,
    PRIMARY KEY((user_id), created_at)
);