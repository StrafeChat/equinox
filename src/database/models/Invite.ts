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
      type: "timestamp",
      cluseringKey: true
    },
    expires_at: {
      type: "int",
    },
  },
  {
    sortBy: {
      column: "created_at",
      order: "DESC",
    },
  }
);

export default new Model("invites", schema);
