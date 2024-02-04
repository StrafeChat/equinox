import { FrozenType, Model, Schema } from "better-cassandra";
import { IRoom } from "../../types";

const schema = new Schema<IRoom>({
    id: {
        type: "text",
        partitionKey: true
    },
    type: {
        type: "text",
    },
    space_id: {
        type: "text"
    },
    position: {
        type: "int"
    },
    permission_overwrites: {
        type: new FrozenType("set<permission_overwrite>")
    },
    name: {
        type: "text"
    },
    topic: {
        type: "text"
    },
    last_message_id: {
        type: "text"
    },
    bitrate: {
        type: "int"
    },
    user_limit: {
        type: "int"
    },
    rate_limit: {
        type: "int"
    },
    recipients: {
        type: "set<text>"
    },
    icon: {
        type: "text"
    },
    owner_id: {
        type: "text"
    },
    parent_id: {
        type: "text"
    },
    last_pin_timestamp: {
        type: "timestamp"
    },
    rtc_region: {
        type: "text"
    },
    created_at: {
        type: "timestamp",
        cluseringKey: false
    },
    edited_at: {
        type: "timestamp"
    }
});

export default new Model("rooms", schema);