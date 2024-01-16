import { FrozenType, UDT, UDTSchema } from "better-cassandra";
import { UserPresence } from "../../types";

const schema = new UDTSchema<UserPresence>({
    status: {
        type: "text"
    },
    status_text: {
        type: "text"
    },
    online: {
        type: "boolean"
    }
});

export default new UDT("user_presence", schema);