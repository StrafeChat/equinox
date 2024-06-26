import { WebSocket, RawData } from "ws";
import { EventEmitter } from "events";

export class SignalingRelay extends EventEmitter {
  socket: WebSocket | null = null;
  host: string = process.env.LIVEKIT_URL!;
  token: string = "";

  hostSocket: WebSocket | null = null;

  messageQueue: (RawData)[] = [];

  constructor(socket: WebSocket, url: string, livekitHost: string = process.env.LIVEKIT_URL!) {
    super();

    this.socket = socket;
    this.host = livekitHost;

    const u = new URL(url, "ws://localhost");
    const params = u.searchParams;
    this.token = params.get("access_token")!;

    const hostUrl = new URL(`/rtc?${params.toString()}`, this.host);
    this.hostSocket = new WebSocket(hostUrl.href);

    this.setupEvents();
  }

  setupEvents(): void {
    const socket = this.socket!;
    const host = this.hostSocket!;
    socket.on("message", (data) => {
      this.sendServerMessage(data);
    });
    host.on("message", (data) => {
      socket.send(data);
    });

    host.on("open", () => {
      this.emptyQueue();
    });

    socket.on("close", () => {
      this.emit("close");
      host.close();
    });
    host.on("close", () => {
      this.emit("close");
      socket.close();
    });
  }

  sendServerMessage(data: RawData): void {
    const host = this.hostSocket!;
    if (host.readyState !== host.OPEN) {
      this.messageQueue.push(data);
      return;
    }

    host.send(data);
  }

  emptyQueue(): void {
    this.messageQueue.forEach(m => {
      this.socket!.send(m);
    });
  }
}
