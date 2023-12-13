CREATE TABLE IF NOT EXISTS {keyspace}.emojis (
    id TEXT,
    name TEXT,
    roles SET<TEXT>,
    user_id TEXT,
    animated BOOLEAN
);