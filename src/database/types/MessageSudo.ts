import { UDT, UDTSchema } from "better-cassandra";
import { MessageSudo } from "../../types";

const schema = new UDTSchema<MessageSudo>({
    name: {
        type: "text"
    },
    avatar_url: {
        type: "text"
    },
    color: {
        type: "text"
    },
});

export default new UDT("message_sudo", schema);