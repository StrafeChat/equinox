-- Voice rooms: per-room user limit (0 = unlimited) and audio bitrate in bits per second
-- (0 = the client default, 64 kbps). Only meaningful on rooms of type 4 (space voice).
ALTER TABLE rooms ADD user_limit int;
ALTER TABLE rooms ADD bitrate int;

-- One-shot data fixes that run from Go rather than CQL (CQL cannot, for instance, OR a
-- bit into every existing role). The API claims a name here with a lightweight
-- transaction before doing the work, so it runs exactly once per keyspace.
CREATE TABLE IF NOT EXISTS data_migrations (
    name text PRIMARY KEY,
    applied_at timestamp
);
