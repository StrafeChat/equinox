import { Model, Schema } from "better-cassandra";
import { IBannedSpaceMember, IMessageByRoom } from "../../types";

const schema = new Schema<IBannedSpaceMember>({
    user_id: {
        type: "text",
        partitionKey: true,
    },
    space_id: {
        type: "text",
        cluseringKey: true
    },
    user_ip: {
        type: "text",
    }
});

export default new Model("banned_space_members", schema);