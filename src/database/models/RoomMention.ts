import { Model, Schema } from "better-cassandra";
import { IRoomMention } from "../../types";

const schema = new Schema<IRoomMention>({
    room_id: {
        type: "text",
        partitionKey: true
    },
    user_id: {
        type: "text",
        partitionKey: true
    },
    message_ids: {
        type: "set<text>",
    },
});

export default new Model("room_mentions", schema);