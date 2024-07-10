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
    name: {
        type: "text"
    },
    icon: {
        type: "text"
    },
    color: {
        type: "text"
    },
    rank: {
        type: "int"
    },
    hoist: {
        type: "boolean"
    },
    allowed_permissions: {
        type: "int"
    },
    denied_permissions: {
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