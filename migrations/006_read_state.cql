-- Read state: per-user per-room ACK pointer and mention count.
-- Unread = messages where id > last_read_message_id.

ALTER TABLE rooms_by_user ADD last_read_message_id bigint;
ALTER TABLE rooms_by_user ADD mention_count int;
