import { UDT, UDTSchema } from "better-cassandra";
import { MessageEmbedField } from "../../types";

const schema = new UDTSchema<MessageEmbedField>({
    name: {
        type: "text"
    },
    value: {
        type: "text"
    },
    inline: {
        type: "boolean"
    }
});

export default new UDT("message_embed_field", schema);