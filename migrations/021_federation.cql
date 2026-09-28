-- Federation: instances talking to each other.
--
-- Remote users get a local "shadow" row in `users` (home_domain + remote_id set, no
-- email/password, not in the username/email lookup tables) so every existing code path
-- that resolves a user id keeps working unchanged. users_by_remote maps a federated
-- identity (@remote_id:home_domain) back to that shadow row.
ALTER TABLE users ADD home_domain text;
ALTER TABLE users ADD remote_id bigint;

CREATE TABLE IF NOT EXISTS users_by_remote (
    home_domain text,
    remote_id bigint,
    user_id bigint,
    PRIMARY KEY (home_domain, remote_id)
);

-- Rooms that span instances. Every participating instance keeps its own local room row
-- (local id, local participants) and these tables tie it to the room's global identity:
-- the (origin_domain, origin_room_id) pair of the instance that created it.
CREATE TABLE IF NOT EXISTS federated_rooms (
    room_id bigint,
    origin_domain text,
    origin_room_id bigint,
    created_at timestamp,
    PRIMARY KEY (room_id)
);

CREATE TABLE IF NOT EXISTS federated_rooms_by_origin (
    origin_domain text,
    origin_room_id bigint,
    room_id bigint,
    PRIMARY KEY (origin_domain, origin_room_id)
);

-- Messages relayed from another instance get a local message id; these map it to the
-- id the sending instance knows it by (for edits, deletes and replies crossing back).
CREATE TABLE IF NOT EXISTS federated_messages (
    room_id bigint,
    message_id bigint,
    origin_domain text,
    origin_message_id bigint,
    PRIMARY KEY (room_id, message_id)
);

CREATE TABLE IF NOT EXISTS federated_messages_by_origin (
    origin_domain text,
    origin_message_id bigint,
    room_id bigint,
    message_id bigint,
    PRIMARY KEY (origin_domain, origin_message_id)
);

-- Peers we have exchanged signed requests with: their current signing key (cached from
-- their /.well-known/strafe) and an operator block flag.
CREATE TABLE IF NOT EXISTS federation_peers (
    domain text,
    key_id text,
    public_key text,
    federation_url text,
    api_url text,
    first_seen timestamp,
    last_seen timestamp,
    blocked boolean,
    PRIMARY KEY (domain)
);
