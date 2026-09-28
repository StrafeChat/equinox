-- room_mention_counts (017_mentions.cql) is a counter, which must never be reset via
-- DELETE/write-zero in normal operation: Cassandra/Scylla counters interact badly with
-- deletes (the delete leaves a tombstone, and an increment that lands before the
-- tombstone's grace period elapses can be silently lost - a documented anti-pattern, not
-- a hypothetical). So "mark as read" no longer deletes the counter row; instead it snapshots
-- the counter's current total here, and the displayed count is (counter total - baseline).
ALTER TABLE rooms_by_user ADD mention_count_baseline bigint;
