import { Room, RoomServiceClient } from 'livekit-server-sdk';

interface RoomManagerOptions {
  key: string;
  secret: string;
  host?: string;
}

interface UserInfo {
  user: string;
  room: string;
  space: string;
}

export class RoomManager {
  host: string = "http://localhost:7880";
  svc: RoomServiceClient | null = null;

  tokens: (UserInfo & {
    token: string,
  })[] = []

  constructor(options: RoomManagerOptions) {
    this.host = options.host || this.host;

    this.svc = new RoomServiceClient(this.host, options.key, options.secret);
  }

  createOrIgnore(roomId: string): Promise<void> {
    return new Promise(async (res, rej) => {
      const rooms = await this.svc?.listRooms();
      const idx = (rooms || []).findIndex((v) => {
        return v.name === roomId
      });
      if (idx !== -1) return res();

      this.svc?.createRoom({
        name: roomId,
        // timeout in seconds
        emptyTimeout: 5 * 60,
        maxParticipants: 20, // TODO: change
      });
    });
  }

  // TODO: IMPORTANT: Gargabe collection of unused tokens
  addToken(token: string, user: string, room: string, space: string): void {
    this.tokens.push({
      token,
      user: user,
      room,
      space
    });
  }
  getUserByToken(token: string): UserInfo | null {
    const data = this.tokens.find(t => {
      return t.token === token
    });
    return data ? {
      user: data.user,
      room: data.room,
      space: data.space
    } : null;
  }
}
