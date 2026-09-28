-- Recovery backups carry the device's Megolm session export as well as the PIN-wrapped
-- store passphrase. Without it a restore only reopened a local IndexedDB store that
-- clearing site data had already deleted, so the engine started an empty account and every
-- past message failed to decrypt.
--
-- Still ciphertext to the server: this column is AES-256-GCM encrypted under a key derived
-- from the store passphrase, which itself only exists inside encrypted_backup.
ALTER TABLE device_key_backups ADD room_keys text;
