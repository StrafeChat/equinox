import { Model, Schema } from "better-cassandra";
import { IVerification } from "../../types";

const schema = new Schema<IVerification>({
    id: {
        type: "text",
        partitionKey: true
    },
    code: {
        type: "text"
    }
});

export default new Model("verifications", schema);