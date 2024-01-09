import { FrozenType, Model, Schema } from "better-cassandra";
import { IUser } from "../../types";

const schema = new Schema<IUser>({
    id: {
        type: "text",
        partitionKey: true,
    },
    accent_color: {
        type: "int"
    },
    avatar: {
        type: "text"
    },
    avatar_decoration: {
        type: "text"
    },
    banned: {
        type: "boolean"
    },
    banner: {
        type: "text"
    },
    bot: {
        type: "boolean"
    },
    created_at: {
        type: "timestamp",
    },
    discriminator: {
        type: "int"
    },
    dob: {
        type: "timestamp"
    },
    edited_at: {
        type: "timestamp"
    },
    email: {
        type: "text"
    },
    flags: {
        type: "int"
    },
    global_name: {
        type: "text"
    },
    last_pass_reset: {
        type: "timestamp"
    },
    locale: {
        type: "text"
    },
    mfa_enabled: {
        type: "boolean"
    },
    password: {
        type: "text"
    },
    premium_type: {
        type: "int"
    },
    presence: {
        type: new FrozenType("user_presence")
    },
    public_flags: {
        type: "int"
    },
    secret: {
        type: "text"
    },
    system: {
        type: "boolean"
    },
    theme: {
        type: "text"
    },
    username: {
        type: "text"
    },
    verified: {
        type: "boolean"
    }
}, { sortBy: { "column": "created_at", order: "DESC" } });

export default new Model("users", schema);