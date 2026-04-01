-- Group setting: owner can disable E2EE for the room (messages stored as plaintext).
-- null/true = E2EE enabled (default); false = E2EE disabled.
ALTER TABLE rooms ADD e2ee_enabled boolean;

-- When E2EE is disabled, server stores plaintext; ciphertext may be empty.
ALTER TABLE messages ADD plaintext text;
