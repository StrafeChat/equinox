-- Room creator (group owner) and system messages for group events.
-- creator_id: set for group PMs; only creator can remove members or rename.
-- system_type / system_payload: server-generated messages (member_added, member_removed, room_renamed).

ALTER TABLE rooms ADD creator_id bigint;
ALTER TABLE messages ADD system_type text;
ALTER TABLE messages ADD system_payload text;
