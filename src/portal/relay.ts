import { WebSocket, RawData } from "ws";
import { EventEmitter } from "events";

export class SignalingRelay extends EventEmitter {
  socket: WebSocket | null = null;
  host: string = process.env.LIVEKIT_URL!;
  token: string = "";

  hostSocket: WebSocket | null = null;

  messageQueue: (RawData)[] = [];

  closing: boolean = false;

  constructor(socket: WebSocket, url: string, livekitHost: string = process.env.LIVEKIT_URL!) {
    super();

    this.socket = socket;
    this.host = livekitHost;

    const u = new URL(url, "ws://localhost");
    const params = u.searchParams;
    this.token = params.get("access_token")!;
    console.log(this.token);

    const hostUrl = new URL(`/rtc?${params.toString()}`, this.host);
    this.hostSocket = new WebSocket(hostUrl.href);

    this.setupEvents();
  }

  setupEvents(): void {
    const socket = this.socket!;
    console.log("socket");
    const host = this.hostSocket!;
    socket.on("message", (data) => {
      console.log("message", data.toString())
      this.sendServerMessage(data);
    });
    host.on("message", (data) => {
      socket.send(data);
    });

    host.on("open", () => {
      this.emptyQueue();
    });

    socket.on("close", () => {
      console.log("socket closed");
      this.close();
    });
    host.on("close", () => {
      console.log("host closed")
      this.close();
    });
  }

  private close() {
    console.log("closing");
    if (this.closing) return;
    this.closing = true;
    if (this.socket?.readyState === WebSocket.OPEN || WebSocket.CONNECTING) this.socket!.close();
    if (this.hostSocket?.readyState === WebSocket.OPEN || WebSocket.CONNECTING) this.hostSocket!.close();
    this.emit("close");
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
