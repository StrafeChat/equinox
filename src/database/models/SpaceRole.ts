import { Model, Schema } from "better-cassandra";
import { ISpaceRole } from "../../types";

const schema = new Schema<ISpaceRole>({
    id: {
        type: "text",
        partitionKey: true
    },
    space_id: {
        type: "text",
        cluseringKey: true
    },
    rank: {
        type: "int"
    },
    hoist: {
        type: "boolean"
    },
    permissions: {
        type: "int"
    },
    edited_at: {
        type: "timestamp"
    },
    created_at: {
        type: "timestamp",
        cluseringKey: true
    },
});

export default new Model("space_roles", schema);