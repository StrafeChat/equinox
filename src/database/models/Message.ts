import { FrozenType, Model, Schema } from "better-cassandra";
import { IMessage } from "../../types";

const schema = new Schema<IMessage>({
    id: {
        type: "text",
        partitionKey: true
    },
    room_id: {
        type: "text",
        cluseringKey: true,
    },
    space_id: {
        type: "text"
    },
    author_id: {
        type: "text"
    },
    content: {
        type: "text"
    },
    system: {
        type: "boolean"
    },
    tts: {
        type: "boolean"
    },
    attachments: {
        type: "set<text>"
    },
    embeds: {
        type: new FrozenType("set<message_embed>")
    },
    flags: {
        type: "int"
    },
    mention_everyone: {
        type: "boolean"
    },
    mention_roles: {
        type: "set<text>"
    },
    mention_rooms: {
        type: "set<text>"
    },
    mentions: {
        type: "set<text>"
    },
    message_reference_id: {
        type: "text"
    },
    pinned: {
        type: "boolean"
    },
    reactions: {
        type: new FrozenType("set<message_reaction>")
    },
    sudo: {
        type: new FrozenType("message_sudo")
    },
    stickers: {
        type: "set<text>"
    },
    thread_id: {
        type: "text"
    },
    webhook_id: {
        type: "text"
    },
    edited_at: {
        type: "timestamp"
    },
    created_at: {
        type: "timestamp",
        cluseringKey: true
    },

}, {
    sortBy: {
        column: "created_at",
        order: "DESC"
    }
});

export default new Model("messages", schema);