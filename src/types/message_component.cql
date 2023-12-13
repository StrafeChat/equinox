CREATE TYPE IF NOT EXISTS {keyspace}.message_component (
    type INT,
    style INT,
    label TEXT,
    emoji TEXT,
    custom_id TEXT,
    url TEXT,
    disabled BOOLEAN
);