CREATE TYPE IF NOT EXISTS {keyspace}.reaction_count_details (
    burst COUNTER,
    normal COUNTER
);