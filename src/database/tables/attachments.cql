CREATE TABLE IF NOT EXISTS {keyspace}.attachments (
    id TEXT,
    filename TEXT,
    description TEXT,
    content_type TEXT,
    size INT,
    url TEXT,
    height INT,
    width INT,
    ephemeral BOOLEAN,
    duration_secs FLOAT,
    waveform TEXT,
    flags INT,
    PRIMARY KEY(id)
);