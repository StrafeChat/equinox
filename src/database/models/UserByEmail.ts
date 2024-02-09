import { Model, Schema } from "better-cassandra";
import { IUserByEmail } from "../../types";

const schema = new Schema<IUserByEmail>({
    id: {
        type: "text",
        partitionKey: false
    },
    email: {
        type: "text",
        partitionKey: true
    }
});

export default new Model("users_by_email", schema);