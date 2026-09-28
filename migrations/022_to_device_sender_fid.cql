-- To-device messages remember the sender's federated id (@<id>:<domain>) so a device
-- polling over REST gets the same sender identity the live gateway push carries. Empty
-- for rows written before federation support (the client falls back to the local id).
ALTER TABLE to_device_messages ADD sender_fid text;
