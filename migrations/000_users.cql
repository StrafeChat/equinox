-- Base user schema: UDT + main users table.
-- Run this in the same keyspace configured as SCYLLA_KEYSPACE (e.g. via: cqlsh -k strafechattest -f migrations/000_users.cql).

CREATE TYPE IF NOT EXISTS user_presence (
    online boolean,
    status text,
    custom_status text
);

CREATE TABLE IF NOT EXISTS users (
    id bigint PRIMARY KEY,
    email text,
    password text,
    username text,
    discriminator int,
    display_name text,
    avatar text,
    banner text,
    bot boolean,
    bots set<text>,
    system boolean,
    bio text,
    flags int,
    relationships set<bigint>,
    spaces set<bigint>,
    date_of_birth timestamp,
    verified_email boolean,
    about_me text,
    accent_color text,
    locale text,
    presence frozen<user_presence>,
    created_at timestamp,
    updated_at timestamp
);

