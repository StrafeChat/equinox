CREATE TABLE IF NOT EXISTS {keyspace}.requests_by_sender (
    sender_id TEXT,
    receiver_id TEXT,
    PRIMARY KEY((sender_id))
);