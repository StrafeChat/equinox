-- Channel permission sync: a space channel can be "synced" to its parent section (category),
-- meaning it follows the section's permission overrides instead of its own (Discord's category
-- sync). Resolved at read time - a synced channel's effective overrides are its section's - so
-- editing a category updates its synced channels with no copying. Null = not synced.
ALTER TABLE rooms ADD permissions_synced boolean;
