import { Model, Schema } from "better-cassandra";
import { IMessageByRoom } from "../../types";

const schema = new Schema<IMessageByRoom>({
    id: {
        type: "text",
        cluseringKey: true
    },
    room_id: {
        type: "text",
        partitionKey: true
    },
}, {
    sortBy: {
        column: "id",
        order: "DESC"
    }
});

export default new Model("messages_by_room", schema);