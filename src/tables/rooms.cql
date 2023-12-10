CREATE TABLE IF NOT EXISTS {keyspace}.rooms (
    id TEXT,
    space_id TEXT,
    icon TEXT,
    name TEXT,
    owner_id TEXT,
    parent_id TEXT,
    position INT,
    recipients SET<TEXT>,
    topic TEXT,
    total_messages_sent INT,
    type INT,
    created_at TIMESTAMP,
    edited_at TIMESTAMP,
    PRIMARY KEY((id), created_at)
) WITH CLUSTERING ORDER BY (created_at DESC);