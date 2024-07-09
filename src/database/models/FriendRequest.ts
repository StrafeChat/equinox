import { Model, Schema } from "better-cassandra";
import { IFriendRequest } from "../../types";

const schema = new Schema<IFriendRequest>({
  id: {
    type: "text",
    partitionKey: true
  },
  sender_id: {
    type: "text",
  },
  recipient_id: {
    type: "text",
  },
  created_at: {
    type: "timestamp",
  }
});

export default new Model("friend_requests", schema);