import { WebSocket } from "ws";

export interface UserWebSocket extends WebSocket {
    id: string;
    alive: boolean;
}