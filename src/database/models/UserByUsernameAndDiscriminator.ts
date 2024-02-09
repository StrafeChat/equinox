import { Model, Schema } from "better-cassandra";
import { IUserByUsernameAndDiscriminator } from "../../types";

const schema = new Schema<IUserByUsernameAndDiscriminator>({
    id: {
        type: "text",
        partitionKey: false
    },
    username: {
        type: "text",
        partitionKey: true
    },
    discriminator: {
        type: "int",
        partitionKey: true
    }
});

export default new Model("users_by_username_and_discriminator", schema);