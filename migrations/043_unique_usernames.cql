-- Usernames become unique on their own: the username#discriminator tag is retired (as on
-- Discord since 2023). This table claims a username case-insensitively - its key is the
-- lowercased username, while the users row keeps the spelling the person chose.
--
-- Filling it, renaming the accounts that shared a name, and dropping the discriminator
-- column plus the users_by_username_discriminator table is done by the API's own data
-- migration (auth.BackfillUniqueUsernames, "043_unique_usernames" in data_migrations) on
-- its first start after this file: the renames reuse each duplicate's old discriminator
-- ("bob#4821" becomes "bob4821"), so the column cannot be dropped here, ahead of it.
CREATE TABLE IF NOT EXISTS users_by_username (
    username text PRIMARY KEY,
    user_id bigint
);
