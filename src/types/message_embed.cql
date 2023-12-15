CREATE TYPE IF NOT EXISTS {keyspace}.message_embed (
    title TEXT,
    description TEXT,
    url TEXT,
    timestamp TIMESTAMP,
    color INT,
    footer FROZEN<message_embed_footer>,
    image FROZEN<message_embed_media>,
    thumbnail FROZEN<message_embed_media>,
    video FROZEN<message_embed_media>,
    author FROZEN<message_embed_author>,
    fields LIST<FROZEN<message_embed_field>>
);