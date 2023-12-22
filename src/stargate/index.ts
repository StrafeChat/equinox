import { RawData, WebSocket, WebSocketServer } from "ws";
import OpCodes from "./OpCodes";
import User from "../interfaces/User";
import Collection from "../utility/Collection";
import Http from "http";
import Https from "https";
import OpHandler from "./OpHandler";
import CloseCodes from "./CloseCodes";

export default class WsHandler {

    public static clients = new Map<WebSocket, { timer: NodeJS.Timeout | null; user: User | null }>();
    public static sockets = new Map<string, WebSocket>();

    private handleConnection = (client: WebSocket) => {
        WsHandler.clients.set(client, {
            timer: setTimeout(async () => {
                client.close(
                    CloseCodes.SESSION_TIMED_OUT,
                    "Your session seems to be invalid, please login!"
                );
            }, parseInt(process.env.HEARTBEAT_INTERVAL!) + 1000), user: null
        });
        WsHandler.send(client, { op: OpCodes.HELLO, data: { heartbeat_interval: parseInt(process.env.HEARTBEAT_INTERVAL!) } });
    }

    private handleClientError = (error: Error) => {
        console.trace(error);
    }

    private handleClientMessage = async (client: WebSocket, message: RawData) => {
        const { op, data }: { op: OpCodes; data: any } = JSON.parse(message.toString("utf-8"));
        switch (op) {
            case OpCodes.HEARTBEAT:
                return OpHandler.handleHeartbeat(client)
            case OpCodes.IDENTIFY:
                return OpHandler.handleIdentify(client, data);
            case OpCodes.PRESENCE_UPDATE:
                return OpHandler.handlePresenceUpdate(client, data);
        }
    }

    private handleClientClose = async (client: WebSocket, code: number, reason: Buffer) => {
        const user = WsHandler.clients.get(client)?.user;
        if (user) {
            const friendList: any[] = [];
            friendList.push(...await Collection.requests.fetchManyReceiverRequests(user.id));
            friendList.push(...await Collection.requests.fetchManySenderRequests(user.id));

            for (const friend of friendList) {
                const userId = user.id == friend.receiver_id ? friend.sender_id : friend.receiver_id;
                const friendSocket = WsHandler.sockets.get(userId);
                if (friendSocket) WsHandler.send(friendSocket, { op: OpCodes.PRESENCE_UPDATE, data: { ...user.presence, online: false, user_id: user.id } })
            }
        }
    }

    constructor(server: Http.Server | Https.Server) {
        const wss = new WebSocketServer({ server: server, path: "/events" });

        wss.on("connection", (client) => {
            this.handleConnection(client);
            client.on("error", this.handleClientError);
            client.on("message", async (message) => await this.handleClientMessage(client, message));
            client.on("close", async (code, reason) => this.handleClientClose(client, code, reason));
        });
    }

    public static send(client: WebSocket, { op, data, event }: { op: OpCodes, data: any, event?: string }) {
        client.send(JSON.stringify({ op, data, event }));
    }
}