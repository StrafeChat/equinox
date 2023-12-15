CREATE TYPE IF NOT EXISTS {keyspace}.message_reaction (
    count INT,
    count_details FROZEN<reaction_count_details>,
    emoji TEXT,
    burst_colors SET<INT>
);