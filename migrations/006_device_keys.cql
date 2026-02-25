-- Signal Protocol (E2EE): device keys and prekeys for X3DH key agreement.
-- Server stores PUBLIC keys only; zero knowledge of plaintext.

-- Device registration: identity key + signed prekey per device.
CREATE TABLE IF NOT EXISTS device_keys (
    user_id bigint,
    device_id bigint,
    identity_key_public text,      -- Curve25519/X25519 public, base64
    signed_prekey_public text,
    signed_prekey_signature text,
    signed_prekey_id int,
    registration_id int,           -- Signal compatibility
    created_at timestamp,
    updated_at timestamp,
    PRIMARY KEY (user_id, device_id)
);

-- One-time prekeys: single-use, consumed (deleted) when used in X3DH.
CREATE TABLE IF NOT EXISTS one_time_prekeys (
    user_id bigint,
    device_id bigint,
    key_id int,
    public_key text,
    created_at timestamp,
    PRIMARY KEY ((user_id, device_id), key_id)
);
