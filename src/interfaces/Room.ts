export default interface Room {
    id: string;
    space_id: string | null;
    icon: string | null;
    name: string | null;
    owner_id: string | null;
    parent_id: string | null;
    position: number | null;
    recipients: string[],
    topic: string | null;
    total_messages_sent: number | null;
    last_message_sent: number | null;
    type: number;
    created_at: Date;
    edited_at: Date;
}