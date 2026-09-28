-- Per-user, per-room notification settings: mute (indefinite or timed) and a notify-mode
-- override, the same "rooms_by_user is per-user room state" home read state already lives
-- in. notify_mode: 0 = use the client's global default for this room type, 1 = all
-- messages, 2 = only @mentions, 3 = nothing. muted_until is a timed mute (e.g. "for 8
-- hours") and is checked lazily against now() - no cleanup job needed; muted is a separate
-- flag for "muted until turned back on".
ALTER TABLE rooms_by_user ADD muted boolean;
ALTER TABLE rooms_by_user ADD muted_until timestamp;
ALTER TABLE rooms_by_user ADD notify_mode int;
