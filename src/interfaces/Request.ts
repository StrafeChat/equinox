import User from "./User";

export default interface Relationship {
    sender_id: string;
    sender: Partial<User> | null;
    receiver_id: string;
    receiver: Partial<User> | null;
    status: string;
    created_at: Date
}