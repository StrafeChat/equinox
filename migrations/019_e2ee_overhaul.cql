-- Replaces the old ad-hoc X3DH device-key model (unsigned, unratcheted) with a real
-- Olm/Megolm-compatible one. The server stores the client's self-signed key claims
-- opaquely and verifies nothing about their cryptographic content itself - per the
-- Signal/Matrix threat model the server is untrusted, so the *client* is what verifies
-- signatures before trusting a bundle. Old rows use an incompatible, unsigned format
-- with no migration path, so this is a clean replace, not an ALTER.

DROP TABLE IF EXISTS device_keys;
DROP TABLE IF EXISTS one_time_prekeys;

-- One row per device. keys_json is the client's self-signed device-keys claim (identity
-- Curve25519 key + Ed25519 fingerprint key + algorithms, signed as one unit by the
-- device's own Ed25519 key) - see the Matrix /keys/upload `device_keys` shape.
CREATE TABLE IF NOT EXISTS device_keys (
    user_id bigint,
    device_id bigint,
    keys_json text,
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY (user_id, device_id)
);

-- One-time and fallback keys, keyed by the "<algorithm>:<key_id>" composite (e.g.
-- "signed_curve25519:AAAAAQ"), each individually signed by the device's Ed25519 key.
-- Fallback keys (is_fallback = true) are never deleted on claim, since they exist
-- specifically to be reused as a last resort once the one-time pool is exhausted.
CREATE TABLE IF NOT EXISTS device_one_time_keys (
    user_id bigint,
    device_id bigint,
    key_id text,
    key_json text,
    is_fallback boolean,
    created_at timestamp,
    PRIMARY KEY ((user_id, device_id), key_id)
);

-- To-device messages: the missing primitive group (Megolm) session key distribution and
-- Olm session setup need - a channel to deliver a small encrypted control message to one
-- specific device, outside the normal chat-message stream. Delivered live over the
-- gateway; this table is the durable fallback for a device that's offline when it's sent
-- (list + ack-delete, same shape read-state already uses for messages).
CREATE TABLE IF NOT EXISTS to_device_messages (
    recipient_user_id bigint,
    recipient_device_id bigint,
    message_id bigint,
    sender_user_id bigint,
    sender_device_id bigint,
    event_type text,
    content_json text,
    created_at timestamp,
    PRIMARY KEY ((recipient_user_id, recipient_device_id), message_id)
) WITH CLUSTERING ORDER BY (message_id ASC);
