-- Lookup tables for users.

-- Login and email uniqueness: partition by email.
CREATE TABLE IF NOT EXISTS users_by_email (
    email text PRIMARY KEY,
    user_id bigint
);

-- Lookup by username#discriminator: partition by username, cluster by discriminator.
CREATE TABLE IF NOT EXISTS users_by_username_discriminator (
    username text,
    discriminator int,
    user_id bigint,
    PRIMARY KEY (username, discriminator)
);
