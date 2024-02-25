import { Model, Schema } from "better-cassandra";
import { IMessageByRoom } from "../../types";

const schema = new Schema<IMessageByRoom>({
    id: {
        type: "text",
        partitionKey: false
    },
    room_id: {
        type: "text",
        partitionKey: true
    }
});

export default new Model("message_by_room", schema);