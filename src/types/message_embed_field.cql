CREATE TYPE IF NOT EXISTS {keyspace}.message_embed_field (
    name TEXT,
    value TEXT,
    inline BOOLEAN
);