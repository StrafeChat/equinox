CREATE TABLE IF NOT EXISTS {keyspace}.users_by_email (
    id TEXT,
    email TEXT,
    PRIMARY KEY((email), id)
) WITH CLUSTERING ORDER BY (id DESC);