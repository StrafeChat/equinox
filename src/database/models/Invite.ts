import { Model, Schema } from "better-cassandra";
import { IInvite } from "../../types";

const schema = new Schema<IInvite>({
    code: {
      type: "text",
      partitionKey: true
    },
    space_id: {
      type: "text",
      cluseringKey: true
    },
    room_id: {
      type: "text",
    },
    vanity: {
      type: "boolean"
    },
    inviter_id: {
      type: "text",
    },
    uses: {
      type: "int"
    },
    max_uses: {
      type: "int"
    },
    created_at: {
      type: "timestamp"
    },
    expires_at: {
      type: "int",
    },
  }
);

export default new Model("invites", schema);
