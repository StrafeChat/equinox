import { UDT, UDTSchema } from "better-cassandra";
import { PermissionOverwrite } from "../../types";

const schema = new UDTSchema<PermissionOverwrite>({
    id: {
        type: "text"
    },
    type: {
        type: "int"
    },
    allow_flags: {
        type: "int"
    },
    deny_flags: {
        type: "int"
    }
});

export default new UDT("permission_overwrite", schema);