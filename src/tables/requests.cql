CREATE TABLE IF NOT EXISTS {keyspace}.requests (
    sender_id TEXT,
    receiver_id TEXT,
    status TEXT,
    created_at TIMESTAMP,
    PRIMARY KEY((receiver_id), created_at)
) WITH CLUSTERING ORDER BY (created_at DESC);