import { RawData, WebSocket, WebSocketServer } from "ws";
import { OpCodes } from "./OpCodes";
import { Validator } from "../utility/Validator";
import { User } from "../interfaces/User";
import { Generator } from "../utility/Generator";
import { cassandra } from "..";
import { Collection } from "../utility/Collection";
import Http from "http";
import Https from "https";
import OpHandler from "./OpHandler";

export class WsHandler {

    public static clients = new Map<WebSocket, { timer: NodeJS.Timeout | null; user: User | null }>();
    public static sockets = new Map<string, WebSocket>();

    private handleConnection = (client: WebSocket) => {
        WsHandler.clients.set(client, {
            timer: setTimeout(async () => {
                client.close(
                    4008,
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
                OpHandler.handleHeartbeat(client)
                break;
            case OpCodes.IDENTIFY:
                const res = await Validator.token(data.token);
                if (res.code) {
                    client.send(JSON.stringify({ op: OpCodes.INVALID_SESSION, data: null }));
                    return client.close(res.code, res.message);
                };

                clearTimeout(WsHandler.clients.get(client)?.timer!);

                WsHandler.clients.set(client, {
                    timer: setTimeout(async () => {
                        client.close(
                            4008,
                            "You couldn't keep up with strafe, please try reconnecting."
                        );
                        const timer = WsHandler.clients.get(client)?.timer;
                        if (timer) clearTimeout(timer);

                        await cassandra.execute(`
                             UPDATE ${cassandra.keyspace}.users
                             SET presence=?
                             WHERE id=? AND created_at=? 
                             `, [{ status: res.user!.presence.status, status_text: res.user!.presence.status_text, online: false }, res.user!.id, res.user!.created_at], { prepare: true });

                        WsHandler.clients.delete(client);
                    }, parseInt(process.env.HEARTBEAT_INTERVAL!) + 1000), user: res.user as unknown as User
                });

                await cassandra.execute(`
                    UPDATE ${cassandra.keyspace}.users
                    SET presence=?
                    WHERE id=? AND created_at=?
                    `, [{ status: res.user!.get("presence").status, status_text: res.user?.get("presence").status_text, online: true }, res.user!.get("id"), res.user!.get("created_at")], { prepare: true });

                res.user!.presence.online = true;

                WsHandler.sockets.set((res.user as unknown as User).id, client);

                const friendList: any[] = [];
                friendList.push(...await Collection.requests.fetchManyReceiverRequests(res.user?.get("id")));
                friendList.push(...await Collection.requests.fetchManySenderRequests(res.user?.get("id")));

                for (const friend of friendList) {
                    const userId = res.user?.get("id") == friend.receiver_id ? friend.sender_id : friend.receiver_id;
                    const friendSocket = WsHandler.sockets.get(userId);
                    if (friendSocket) WsHandler.send(friendSocket, { op: OpCodes.PRESENCE_UPDATE, data: { ...res.user?.get("presence"), user_id: res.user?.get("id") } })
                }

                WsHandler.send(client, { op: OpCodes.DISPATCH, data: Generator.stripUserInfo(res.user as unknown as User), event: "READY" })
                break;
            case OpCodes.PRESENCE_UPDATE:
                const user = WsHandler.clients.get(client)!.user;
                const friends: any[] = [];
                console.log(user);
                if (user) {
                    await cassandra.execute(`
                        UPDATE ${cassandra.keyspace}.users
                        SET presence=?
                        WHERE id=? AND created_at=?
                        `, [{ ...user.presence, ...data }, user.id, user.created_at], { prepare: true });

                    friends.push(...await Collection.requests.fetchManyReceiverRequests(user.id));
                    friends.push(...await Collection.requests.fetchManySenderRequests(user.id));

                    for (const friend of friends) {
                        const userId = user.id == friend.receiver_id ? friend.sender_id : friend.receiver_id;
                        const friendSocket = WsHandler.sockets.get(userId);
                        if (friendSocket) WsHandler.send(friendSocket, { op: OpCodes.PRESENCE_UPDATE, data: { ...user.presence, status: data.status, status_text: data.status_text ?? user.presence.status_text, user_id: user.id } })
                    }
                }
                break;
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
        const wss = new WebSocketServer({ server: server });

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