CREATE TYPE IF NOT EXISTS {keyspace}.user_presence (
    status TEXT,
    status_text TEXT,
    online BOOLEAN,
);