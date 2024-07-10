import { Model, Schema } from "better-cassandra";
import { IFriendRequestByUser } from "../../types";

const schema = new Schema<IFriendRequestByUser>({
  sender_id: {
    type: "text",
    partitionKey: true,
  },
  recipient_id: {
    type: "text",
  },
  id: {
    type: "text",
  },
});

export default new Model("friend_requests_by_sender", schema);