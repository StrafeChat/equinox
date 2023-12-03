import { Server } from "http";
import { WebSocket, WebSocketServer } from "ws";
import { OpCodes } from "./OpCodes";
import { Validator } from "../utility/Validator";
import { User } from "../interfaces/User";
import { Generator } from "../utility/Generator";

export class WsHandler {

    public static clients = new Map<WebSocket, { timer: NodeJS.Timeout | null; user: User | null }>();
    public static sockets = new Map<string, WebSocket>();

    constructor(server: Server) {
        const wss = new WebSocketServer({ server });

        wss.on("connection", (client) => {
            WsHandler.clients.set(client, {
                timer: setTimeout(() => {
                    client.close(
                        4008,
                        "You couldn't keep up with strafe, please try reconnecting."
                    );
                    WsHandler.clients.delete(client);
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
                        this.send(client, { op: OpCodes.DISPATCH, data: Generator.stripUserInfo(res.user as unknown as User), event: "READY" })
                        break;
                }
            });
            // client.on("message", async (message) => {
            //     const { op, data }: { op: OpCodes; data: any } = JSON.parse(message.toString("utf-8"));
            //     switch (op) {
            //         case OpCodes.HEARTBEAT:
            //             WsHandler.clients.get(client)?.timer?.refresh();
            //             this.send(client, { op: OpCodes.HEARTBEAT_ACK, data: null });
            //             break;
            //         case OpCodes.IDENTIFY:
            //             const validation = await Validator.token(data.token);
            //             if (validation.code) return client.close(validation.code, validation.message);
            //             if (!validation.user) return;

            //             WsHandler.clients.set(client, {
            //                 timer: setTimeout(async () => {
            //                     client.close(
            //                         4008,
            //                         "You couldn't keep up with strafe, please try reconnecting."
            //                     );
            //                     const user = WsHandler.clients.get(client)?.user;
            //                     // if (!user) return;
            //                     // user.status = {
            //                     //     name: user.status.name,
            //                     //     online: false,
            //                     // };
            //                     // await user?.save();
            //                     WsHandler.clients.delete(client);
            //                 }, parseInt(process.env.HEATBEAT_INTERVAL!) + 1000),
            //                 user: validation.user as any,
            //             });

            //             WsHandler.sockets.set(validation.user.get("id"), client);

            //             // client.send(
            //             //     JSON.stringify({
            //             //       op: OpCodes.DIS,
            //             //       data: {
            //             //         id: user?.id,
            //             //         accentColor: user?.accentColor,
            //             //         avatar: user?.avatar,
            //             //         banner: user?.banner,
            //             //         bot: user?.bot,
            //             //         createdAt: user?.createdAt,
            //             //         displayName: user?.displayName,
            //             //         email: user?.email,
            //             //         locale: user?.locale,
            //             //         preferences: user?.preferences,
            //             //         status: user?.status,
            //             //         tag: user?.tag,
            //             //         username: user?.username,
            //             //       },
            //             //       event: "READY",
            //             //     })
            //             //   );
            //             this.send(client, {
            //                 op: OpCodes.DISPATCH, data: {
            //                     user: {
            //                         ...Generator.rowToObj(validation.user)
            //                     }
            //                 }
            //             })
            //             break;
            //         case OpCodes.HEARTBEAT_ACK:

            //             break;
            //         // TODO: Work on op codes.
            //     }
            // })
        });

    }

    public send(client: WebSocket, { op, data, event }: { op: OpCodes, data: any, event?: string }) {
        client.send(JSON.stringify({ op, data, event }));
    }
}