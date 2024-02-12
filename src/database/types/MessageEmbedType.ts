import { FrozenType, UDT, UDTSchema } from "better-cassandra";
import { MessageEmbed } from "../../types";

const schema = new UDTSchema<MessageEmbed>({
    title: {
        type: "text"
    },
    description: {
        type: "text"
    },
    color: {
        type: "int"
    },
    url: {
        type: "int"
    },
    timestamp: {
        type: "timestamp"
    },
    author: {
        type: new FrozenType("message_embed_author")
    },
    footer: {
        type: new FrozenType("message_embed_footer")
    },
    image: {
        type: new FrozenType("message_embed_media")
    },
    thumbnail: {
        type: new FrozenType("message_embed_media")
    },
    video: {
        type: new FrozenType("message_embed_media")
    },
    fields: {
        type: new FrozenType("set<message_embed_field>")
    }
});

export default new UDT("message_embed", schema);