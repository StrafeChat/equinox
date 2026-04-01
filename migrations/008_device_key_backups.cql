-- Encrypted key backup for Signal protocol key recovery.
-- Server stores ciphertext only; cannot decrypt without user's recovery PIN.
-- Enables cross-device and new-session message decryption
CREATE TABLE IF NOT EXISTS device_key_backups (
    user_id bigint PRIMARY KEY,
    encrypted_backup text,   -- AES-256-GCM(keys, derived from PIN)
    salt text,               -- Random salt for PBKDF2 (base64)
    created_at timestamp,
    updated_at timestamp
);
