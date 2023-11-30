import { Server } from "http";
import { WebSocket, WebSocketServer } from "ws";
import { OpCodes } from "./OpCodes";

export class WsHandler {

    public static clients = new Map<WebSocket, { timer: NodeJS.Timeout | null; user: any | null }>();
    public static sockets = new Map<string, WebSocket>();

    constructor(server: Server) {
        const wss = new WebSocketServer({ server });

        wss.on("connection", (client) => {
            WsHandler.clients.set(client, { timer: null, user: null });
            this.send(client, { op: OpCodes.HEARTBEAT });
            client.on("error", console.error);
            client.on("message", (message) => {
                const { op, data }: { op: OpCodes; data: any } = JSON.parse(message.toString("utf-8"));
                switch(op) {
                    // TODO: Work on op codes.
                }
            })
        });

    }

    public send(client: WebSocket, { op }: { op: OpCodes }) {
        client.send(JSON.stringify({ op }));
    }
}