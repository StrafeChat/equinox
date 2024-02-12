import { UDT, UDTSchema } from "better-cassandra";
import { MessageEmbedAuthor } from "../../types";

const schema = new UDTSchema<MessageEmbedAuthor>({
    name: {
        type: "text"
    },
    url: {
        type: "text"
    },
    icon_url: {
        type: "text"
    }
});

export default new UDT("message_embed_author", schema);