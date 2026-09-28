-- Mention detection: which users a message pings, and a per-room mention counter.
--
-- rooms_by_user.mention_count (006_read_state.cql) is a plain int, which CQL cannot
-- increment in place (`SET x = x + 1` only works on `counter` columns, and those must
-- live in their own table with no other non-key columns). room_mention_counts is that
-- table; rooms_by_user.mention_count is left in place but no longer treated as source
-- of truth by application code - resetting a user's count for a room is a DELETE here,
-- not a write of 0.

ALTER TABLE messages ADD mentions set<bigint>;
ALTER TABLE messages ADD mention_everyone boolean;
ALTER TABLE messages ADD mention_roles set<bigint>;

CREATE TABLE IF NOT EXISTS room_mention_counts (
  user_id bigint,
  room_id bigint,
  count counter,
  PRIMARY KEY (user_id, room_id)
);
