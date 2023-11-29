CREATE TABLE IF NOT EXISTS strafe.users_by_email (
    id TEXT,
    email TEXT,
    PRIMARY KEY((email), id)
) WITH CLUSTERING ORDER BY (id DESC);