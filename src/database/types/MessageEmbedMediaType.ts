import { UDT, UDTSchema } from "better-cassandra";
import { MessageEmbedMedia } from "../../types";

const schema = new UDTSchema<MessageEmbedMedia>({
    url: {
        type: "text"
    },
    height: {
        type: "int"
    },
    width: {
        type: "int"
    }
});

export default new UDT("message_embed_media", schema);