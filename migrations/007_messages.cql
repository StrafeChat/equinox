-- E2EE messages: server stores ciphertext.
-- Client encrypts with Signal Protocol (Double Ratchet) before upload.

-- Messages by room: partition by room, cluster by id for ordering.
CREATE TABLE IF NOT EXISTS messages (
    room_id bigint,
    id bigint,
    sender_id bigint,
    sender_device_id bigint,
    ciphertext text,               -- AEAD ciphertext (ChaCha20-Poly1305)
    reply_to_id bigint,
    created_at timestamp,
    updated_at timestamp,
    deleted_at timestamp,          -- soft delete: set when message removed
    PRIMARY KEY (room_id, id)
) WITH CLUSTERING ORDER BY (id DESC);
