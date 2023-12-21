CREATE TABLE IF NOT EXISTS {keyspace}.requests_by_receiver (
    sender_id TEXT,
    receiver_id TEXT,
    status TEXT,
    created_at TIMESTAMP,
    PRIMARY KEY((receiver_id), sender_id, created_at)
);