import { UDT, UDTSchema } from "better-cassandra";
import { MessageEmbedFooter } from "../../types";

const schema = new UDTSchema<MessageEmbedFooter>({
    text: {
        type: "text"
    },
    icon_url: {
        type: "text"
    }
});

export default new UDT("message_embed_footer", schema);