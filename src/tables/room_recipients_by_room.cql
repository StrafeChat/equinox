CREATE TABLE IF NOT EXISTS {keyspace}.room_recipients_by_room (
    room_id TEXT,
    user_id TEXT,
    created_at TIMESTAMP,
    PRIMARY KEY((room_id), created_at)
) WITH CLUSTERING ORDER BY (created_at DESC);