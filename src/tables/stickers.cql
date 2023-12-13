CREATE TABLE IF NOT EXISTS {keyspace}.stickers (
    id TEXT,
    pack_id TEXT,
    name TEXT,
    description TEXT,
    tags TEXT,
    type INT,
    format_type INT,
    available BOOLEAN,
    space_id TEXT,
    user_id TEXT,
    sort_value INT,
    PRIMARY KEY(id)
);