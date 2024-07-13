const { EventEmitter } = require('events');
import WebSocket, { WebSocketServer } from "ws";

import { redis } from "../database";

class P2PCall extends EventEmitter {
  caller: P2PUser;
  recipient: P2PUser | null = null;

  constructor(caller: P2PUser) {
    super();

    this.caller = caller;
  }

  public async start(user: P2PUser): Promise<void> {
    this.recipient = user;

    this.setupEvents();

    await Promise.all([ // set roles for peers
      this.caller.setImpolite(),
      this.recipient.setPolite(),
    ]);

  }

  private setupEvents() {
    if (!this.recipient) throw new Error("Recipient not set yet.");

    this.caller.on("negotiation", (data: any) => {
      console.log("sending negotiation to recipient");
      this.recipient!.send(OP.NEGOTIATION, data);
    });
    this.recipient!.on("negotiation", (data: any) => {
      console.log("sending negotiation to caller");
      this.caller!.send(OP.NEGOTIATION, data);
    });

    // TODO: handle closing better
    this.caller.on("close", (reason: WSErrorReason) => {
      this.recipient!.terminate(reason);
      this.emit("close");
    });
    this.recipient!.on("close", (reason: WSErrorReason) => {
      this.caller.terminate(reason);
      this.emit("close");
    });
  }

  public setCaller(user: P2PUser) {
    this.caller = user;
    // TODO: setup events
  }
}

interface WSErrorReason {
  message: string;
  code: number;
}

class WSError {
  static INVALID_JSON = {
    message: "400: Invalid JSON",
    code: 4000
  };
  static INVALID_DATA = {
    message: "Invalid data provided or missing fields.",
    code: 4001
  };
  static FORBIDDEN = {
    message: "You are not allowed to perform this action.",
    code: 403
  };
}

enum OP {
  IDENTIFY = 0,
  ACK = 1, // confirmation of a message
  SETTINGS = 2,
  NEGOTIATION = 3,

  ERROR = 20,
}

interface Payloads {
  [OP.IDENTIFY]: { token: string, id: string }
  [OP.ACK]: { id: number }
  [OP.SETTINGS]: { role: "polite" | "impolite", setting: "role" }
}

class P2PUser extends EventEmitter {
  public id: string | null = null;
  public joinToken: string | null = null;
  public ws: WebSocket;

  public server: SignalingServer;

  keepAliveTime: number = 30000; // send a keepAlive message every 30 seconds

  public initiated: boolean = false;

  public role: "polite" | "impolite" | null = null;
  
  /**
   * Initiated after the socket has identified itself.
   * The RoomId is generated by sorting the caller and recipient ids (the order doesn't matter, it just needs to be consistent) and joined with a colon.
   *
   * @type {(string | null)}
   */
  public targetRoom: string | null = null;

  // jobs with their promise resolve functions
  private jobQueue = new Map<number, (data: any) => void>();
  private currJobId = 0;

  constructor(ws: WebSocket, server: SignalingServer) {
    super();

    this.ws = ws;
    this.server = server;

    this.setupEvents();
  }

  private setupEvents() {
    const ws = this.ws;
    ws.on("message", (data: WebSocket.RawData) => {
      var parsed = null;
      try {
        parsed = JSON.parse(data.toString());
      } catch(e) {
        return this.terminate(WSError.INVALID_JSON);
      }

      if (!parsed.op && parsed.op !== 0) return this.terminate(WSError.INVALID_DATA);

      this.parseMessage(parsed as { op: number, data?: any });
    });
  }

  public get friend(): string | null {
    if (!this.targetRoom) return null;
    const ids = this.targetRoom.split(":");
    return ids[0] === this.id ? ids[1] : ids[0];
  }

  async parseMessage(data: { op: OP, data?: any }) { // TODO: types
    switch (data.op) {
      case OP.IDENTIFY:
        this.token = data.data.token as string;
        this.id = data.data.id as string;

        if (!this.token || !this.id) return this.terminate(WSError.INVALID_DATA);

        const userData = this.server.tokens.verifyToken(this.id, this.token);

        if (!userData) return this.terminate(WSError.FORBIDDEN);
        
        this.targetRoom = userData;
        this.initiated = true;

        this.emit("ready");

        // TODO: keepAlive signal
      break;
      case OP.ACK:
        const jobId = data.data.id;
        if (!this.jobQueue.has(jobId)) return;
        this.jobQueue.get(jobId)!(data.data); // call callback function
      break;
      case OP.NEGOTIATION:
        this.emit("negotiation", data.data);
      break;
      default:
        this.send(OP.ERROR, { message: "Invalid OP code." });
      break;
    }
  }

  public async setPolite(): Promise<void> {
    this.role = "polite";
    // TODO: possibly update complete settings state here
    await this.sendWithCallback(OP.SETTINGS, { role: "polite", setting: "role" })
  }
  public async setImpolite(): Promise<void> {
    this.role = "impolite";
    await this.sendWithCallback(OP.SETTINGS, { role: "impolite", setting: "role" })
  }

  sendWithCallback(op: OP, data: any): Promise<any> {
    return new Promise((res, rej) => {
      const jobId = this.currJobId++;
      this.jobQueue.set(jobId, res);
      this.send(op, { ...data, id: jobId });

      // TODO: implement timeouts
    })
  }

  send(op: OP, data?: any) {
    this.ws.send(JSON.stringify({ op, data }));
  }

  terminate(reason: WSErrorReason) {
    this.ws.close(reason.code, reason.message);
    this.emit("close", reason);
  }
}

export interface TokenDetails { user: string, room: string, removeTimeout: NodeJS.Timeout }
export class TokenManager {
  tokens: Map<string, TokenDetails> = new Map();

  users: Map<string, string[]> = new Map();

  constructor() {

  }
  
  /**
   * verifies a join token and returns the room id if it's valid.
   *
   * @param {string} user The user id of the user trying to use the token
   * @param {string} token The token to verify
   * @returns {(string | null)}
   */
  verifyToken(user: string, token: string): string | null {
    if (this.tokens.has(token)) {
      const data = this.tokens.get(token)!;
      if (data.user === user) return data.room;
      return null;
    }
    return null;
  }

  grantToken(user: string, room: string): string {
    if (this.users.has(user)) {
      const ts = this.users.get(user);
      for (let i = 0; i < (ts || []).length; i++) {
        const t = ts![i];
        const token = this.tokens.get(t)!;
        if (token.room === room) {
          return t;
        }
      }
    }

    const token = Math.random().toString(36).substring(2);
    this.users.set(user, [...(this.users.get(user) || []), token]);
    this.tokens.set(token, { user, room, removeTimeout: setTimeout(() => {
      this.tokens.delete(token);
      this.users.set(user, (this.users.get(user) || []).filter(t => t !== token));
    }, 5*1000*60) });
    return token;
  }

  generateRoomId(user: string, friend: string) {
    return [user, friend].sort().join(":");
  }
}

export class SignalingServer {
  public calls = new Map<string, P2PCall>();

  public tokens = new TokenManager();

  constructor(server: WebSocketServer) {

    server.on("connection", (ws: WebSocket) => {
      const user = new P2PUser(ws, this);
      user.on("ready", () => {
        this.createCall(user);
      });
    });
  }

  createCall(user: P2PUser): void {
    if (!user.initiated) throw new Error("Signaling Channel not initiated yet.");
    const roomId = user.targetRoom;
    if (!this.calls.has(roomId!)) {
      console.log("creating call");
      this.callUser(user.friend!, user.id!);
      const call = new P2PCall(user);
      call.on("close", () => {
        this.calls.delete(roomId!);
      });
      this.calls.set(roomId!, call);
      return;
    }
    const call = this.calls.get(roomId!)!;
    if (call.caller.id === user.id) return call.setCaller(user); // wait for the actual recipient to join

    call.start(user); // TODO: manage reconnections properly
  }
  
  callUser(userId: string, caller: string) {
    console.log("calling user");
    const token = this.tokens.grantToken(userId, this.tokens.generateRoomId(userId, caller));
    redis.publish("stargate_personal", JSON.stringify({ event: "call_init", users: [userId], data: { caller, token } }));
  }
}