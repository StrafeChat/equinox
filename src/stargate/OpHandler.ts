import { WebSocket } from "ws";
import WsHandler from ".";
import OpCodes from "./OpCodes";
import Validator from "../utility/Validator";
import Collection from "../utility/Collection";
import User, { UserPresence } from "../interfaces/User";
import Relationship from "../interfaces/Request";
import Generator from "../utility/Generator";
import CloseCodes from "./CloseCodes";

interface IdentifyData {
    token?: string;
}

export default class OpHandler {

    public static handleHeartbeat = (client: WebSocket) => {
        WsHandler.send(client, { op: OpCodes.HEARTBEAT_ACK, data: null })
        WsHandler.clients.get(client)?.timer?.refresh();
    }

    public static handleIdentify = async (client: WebSocket, data: IdentifyData) => {
        const res = await Validator.token(data.token);

        if (res.code) {
            client.send(JSON.stringify({ op: OpCodes.INVALID_SESSION, data: null }));
            return client.close(res.code, res.message);
        };

        clearTimeout(WsHandler.clients.get(client)?.timer!);

        WsHandler.clients.set(client, {
            timer: setTimeout(async () => {
                client.close(
                    CloseCodes.SESSION_TIMED_OUT,
                    "You couldn't keep up with strafe, please try reconnecting."
                );

                const timer = WsHandler.clients.get(client)?.timer;
                if (timer) clearTimeout(timer);

                await Collection.updateTable<User>("users", {
                    update: [
                        { name: "presence", value: { ...res.user!.presence, online: false } },
                        { name: "edited_at", value: Date.now() }
                    ],
                    where: [
                        { name: "id", value: res.user!.id },
                        { name: "created_at", value: res.user!.created_at }
                    ]
                });

                WsHandler.clients.delete(client);
            }, parseInt(process.env.HEARTBEAT_INTERVAL!) + 1000), user: res.user as unknown as User
        });

        await Collection.updateTable<User>("users", {
            update: [
                { name: "presence", value: { ...res.user!.presence, online: true } },
                { name: "edited_at", value: Date.now() }
            ],
            where: [
                { name: "id", value: res.user!.id },
                { name: "created_at", value: res.user!.created_at }
            ]
        });

        res.user!.presence.online = true;

        WsHandler.sockets.set((res.user as unknown as User).id, client);

        const friendList: Relationship[] = [];
        friendList.push(...await Collection.requests.fetchManyReceiverRequests(res.user!.id));
        friendList.push(...await Collection.requests.fetchManySenderRequests(res.user!.id));

        for (const friend of friendList) {
            const userId = res.user!.id == friend.receiver_id ? friend.sender_id : friend.receiver_id;
            const friendSocket = WsHandler.sockets.get(userId);
            if (friendSocket) WsHandler.send(friendSocket, { op: OpCodes.PRESENCE_UPDATE, data: { ...res.user!.presence, user_id: res.user!.id } })
        }

        WsHandler.send(client, { op: OpCodes.DISPATCH, data: Generator.stripUserInfo(res.user!), event: "READY" })
    }

    public static handlePresenceUpdate = async (client: WebSocket, data: UserPresence) => {
        const user = WsHandler.clients.get(client)!.user;
        const friends: Relationship[] = [];
        console.log(user);
        if (user) {
            await Collection.updateTable<User>("users", {
                update: [
                    { name: "presence", value: { ...user.presence, ...data } },
                    { name: "edited_at", value: Date.now() }
                ],
                where: [
                    { name: "id", value: user.id },
                    { name: "created_at", value: user.created_at }
                ]
            })

            friends.push(...await Collection.requests.fetchManyReceiverRequests(user.id));
            friends.push(...await Collection.requests.fetchManySenderRequests(user.id));

            for (const friend of friends) {
                const userId = user.id == friend.receiver_id ? friend.sender_id : friend.receiver_id;
                const friendSocket = WsHandler.sockets.get(userId);
                if (friendSocket) WsHandler.send(friendSocket, { op: OpCodes.PRESENCE_UPDATE, data: { ...user.presence, status: data.status, status_text: data.status_text ?? user.presence.status_text, user_id: user.id } })
            }
        }
    }
}