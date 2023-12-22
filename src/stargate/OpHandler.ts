import { WebSocket } from "ws";
import { WsHandler } from ".";
import { OpCodes } from "./OpCodes";

export default class OpHandler {

    public static handleHeartbeat = (client: WebSocket) => {
        WsHandler.send(client, { op: OpCodes.HEARTBEAT_ACK, data: null })
        WsHandler.clients.get(client)?.timer?.refresh();
    }
}