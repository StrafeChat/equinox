import { Model, Schema } from "better-cassandra";
import { IRoomUnreads } from "../../types";

const schema = new Schema<IRoomUnreads>({
    room_id: {
        type: "text",
        partitionKey: true
    },
    user_id: {
        type: "text",
        partitionKey: true
    },
    message_id: {
        type: "text",
    },
});

export default new Model("room_unreads", schema);