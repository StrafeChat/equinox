import { UDT, UDTSchema } from "better-cassandra";
import { MessageAttachment } from "../../types";

const schema = new UDTSchema<MessageAttachment>({
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

export default new UDT("message_attachment", schema);