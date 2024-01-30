import { Schema } from "better-cassandra";
import { ISpace } from "../../types";

// TODO: Decide what else we want to add with team
const schema = new Schema<ISpace>({
    id: {
        type: "text",
        partitionKey: true
    },
    name: {
        type: "text"
    },
    icon: {
        type: "text"
    },
    owner_id: {
        type: "text"
    },
    afk_room_id: {
        type: "text"
    },
    afk_timeout: {
        type: "int"
    },
    verifcation_level: {
        type: "int"
    },
    room_ids: {
        type: "list<text>"
    },
    role_ids: {
        type: "list<text>"
    },
    rules_room_id: {
        type: "text"
    },
    description: {
        type: "text"
    },
    banner: {
        type: "text"
    },
    preferred_locale: {
        type: "text"
    },
    sticker_ids: {
        type: "list<text>"
    },
    created_at: {
        type: "timestamp",
        cluseringKey: true,
    },
    edited_at: {
        type: "timestamp"
    }
})