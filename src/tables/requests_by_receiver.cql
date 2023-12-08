CREATE TABLE IF NOT EXISTS {keyspace}.requests_by_receiver (
    sender_id TEXT,
    receiver_id TEXT,
    status TEXT,
    PRIMARY KEY((receiver_id))
);