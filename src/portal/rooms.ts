import { RoomServiceClient } from 'livekit-server-sdk';

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
  host: string = process.env.LIVEKIT_URL!;
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
    // TODO: fix double calls from react frontend
    if (!!this.tokens.find(e => e.token === token && e.user === user && e.room === room && e.space === space)) return;
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
