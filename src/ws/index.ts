import { Server } from "http";
import { WebSocket, WebSocketServer } from "ws";
import { OpCodes } from "./OpCodes";
import { Validator } from "../utility/Validator";
import { User } from "../interfaces/User";
import { Generator } from "../utility/Generator";
import { cassandra } from "..";

export class WsHandler {

    public static clients = new Map<WebSocket, { timer: NodeJS.Timeout | null; user: User | null }>();
    public static sockets = new Map<string, WebSocket>();

    constructor(server: Server) {
        const wss = new WebSocketServer({ server }); // WebSocketServer({ server, path: "/events" }); :pleading_face:

        wss.on("connection", (client) => {
            WsHandler.clients.set(client, {
                timer: setTimeout(async () => {
                    client.close(
                        4008,
                        "You couldn't keep up with strafe, please try reconnecting."
                    );
                }, parseInt(process.env.HEARTBEAT_INTERVAL!) + 1000), user: null
            });
            this.send(client, { op: OpCodes.HELLO, data: { heartbeat_interval: parseInt(process.env.HEARTBEAT_INTERVAL!) } });

            client.on("error", console.error);
            client.on("message", async (message) => {
                const { op, data }: { op: OpCodes; data: any } = JSON.parse(message.toString("utf-8"));
                switch (op) {
                    case OpCodes.HEARTBEAT:
                        this.send(client, { op: OpCodes.HEARTBEAT_ACK, data: null })
                        WsHandler.clients.get(client)?.timer?.refresh();
                        break;
                    case OpCodes.IDENTIFY:
                        const res = await Validator.token(data.token);
                        if (res.code) return client.close(res.code, res.message);

                        clearTimeout(WsHandler.clients.get(client)?.timer!);

                        WsHandler.clients.set(client, {
                            timer: setTimeout(async () => {
                                client.close(
                                    4008,
                                    "You couldn't keep up with strafe, please try reconnecting."
                                );

                                const user = WsHandler.clients.get(client)?.user;
                                const timer = WsHandler.clients.get(client)?.timer;
                                if (timer) clearTimeout(timer);

                                if (user) {
                                    await cassandra.execute(`
                                 UPDATE ${cassandra.keyspace}.users
                                 SET presence=?
                                 WHERE id=? AND created_at=? 
                                 `, [{ status: user.presence.status, status_text: user.presence.status_text, online: false }, user.id, user.created_at], { prepare: true });
                                    WsHandler.clients.delete(client);
                                }
                            }, parseInt(process.env.HEARTBEAT_INTERVAL!) + 1000), user: res.user as unknown as User
                        });

                        await cassandra.execute(`
                        UPDATE ${cassandra.keyspace}.users
                        SET presence=?
                        WHERE id=? AND created_at=?
                        `, [{ status: res.user!.get("presence").status, status_text: res.user?.get("presence").status_text, online: true }, res.user!.get("id"), res.user!.get("created_at")], { prepare: true });

                        res.user!.presence.online = true;

                        WsHandler.sockets.set((res.user as unknown as User).id, client);

                        this.send(client, { op: OpCodes.DISPATCH, data: Generator.stripUserInfo(res.user as unknown as User), event: "READY" })
                        break;
                }
            });

            client.on("close", async () => {

            });
        });
    }

    public send(client: WebSocket, { op, data, event }: { op: OpCodes, data: any, event?: string }) {
        client.send(JSON.stringify({ op, data, event }));
    }
}