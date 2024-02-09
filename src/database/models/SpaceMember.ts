import { Model, Schema } from "better-cassandra";
import { ISpaceMember } from "../../types";

const schema = new Schema<ISpaceMember>({
    user_id: {
        type: "text",
        cluseringKey: true
    },
    space_id: {
        type: "text",
        partitionKey: true
    },
    nick: {
        type: "text"
    },
    roles: {
        type: "set<text>"
    },
    joined_at: {
        type: "timestamp"
    },
    deaf: {
        type: "boolean"
    },
    mute: {
        type: "boolean"
    },
    avatar: {
        type: "text"
    },
    edited_at: {
        type: "timestamp"
    },
});

export default new Model("space_members", schema);