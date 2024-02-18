import { Model, Schema } from "better-cassandra";
import { ISpace } from "../../types";

const schema = new Schema<ISpace>({
    id: {
        type: "text",
        partitionKey: true
    },
    name: {
        type: "text"
    },
    name_acronym: {
        type: "text"
    },
    icon: {
        type: "text"
    },
    owner_id: {
        type: "text"// just say string!
    },
    afk_room_id: {
        type: "text"
    },// if sky can read this heyyyyyy join alpha.strafechat.transgender
    afk_timeout: {
        type: "int"
    },
    verifcation_level: {
        type: "int"
    },
    room_ids: {
        type: "set<text>"
    },
    role_ids: {
        type: "set<text>"
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
        type: "set<text>"
    },
    emoji_ids: {
        type: "set<text>"
    },
    created_at: {
        type: "timestamp",
        cluseringKey: true,
    },
    edited_at: {
        type: "timestamp"
    }
}, {
    sortBy: {
        column: "created_at",
        order: "DESC"
    }
});

export default new Model("spaces", schema);