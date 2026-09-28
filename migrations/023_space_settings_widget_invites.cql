-- Space settings: server widget (public member/online counts + an invite) and the
-- multi-use / expiring invite fields that 013 declared but nothing wrote. Invites are now
-- written to both space_invites (per-space listing) and space_invite_by_code (join lookup).

ALTER TABLE spaces ADD widget_enabled boolean;
ALTER TABLE spaces ADD widget_room_id bigint;
ALTER TABLE spaces ADD widget_invite_code text;
