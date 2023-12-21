CREATE MATERIALIZED VIEW IF NOT EXISTS {keyspace}.messages_by_room AS
SELECT * FROM {keyspace}.messages
WHERE room_id IS NOT NULL AND created_at IS NOT NULL
PRIMARY KEY (room_id, id, created_at);