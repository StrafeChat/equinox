import { UDT, UDTSchema } from "better-cassandra";
import { MessageReaction } from "../../types";

const schema = new UDTSchema<MessageReaction>({
    emoji: {
        type: "text"
    },
    user_ids: {
        type: "set<text>"
    }
});

export default new UDT("message_reaction", schema);