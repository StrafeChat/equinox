CREATE TABLE IF NOT EXISTS {keyspace}.requests_by_sender (
    sender_id TEXT,
    receiver_id TEXT,
    status TEXT,
    created_at TIMESTAMP,
    PRIMARY KEY(sender_id, receiver_id, created_at)
);