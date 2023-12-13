CREATE TABLE IF NOT EXISTS {keyspace}.sticker_packs (
    id TEXT,
    stickers SET<TEXT>,
    PRIMARY KEY (id)
);