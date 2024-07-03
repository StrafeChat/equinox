import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import { JoinBody } from "../../types/";
import Room from "../../database/models/Room";

import { AccessToken } from 'livekit-server-sdk';
import { RoomManager } from "../../portal";

const {
  LIVEKIT_API_KEY: key,
  LIVEKIT_API_SECRET: secret
} = process.env;

if (!key || !secret) {
  throw "Livekit api key or secret not set";
}

const router = Router();
router.use(verifyToken);

router.post<{}, {}, JoinBody>("/join", async (req, res) => {
  const room = req.body.roomId;
  const user = res.locals.user;

  console.log("join");

  const mgr = ((req as any).portal.manager as RoomManager); // TODO: add typings
  
  const rooms = await Room.select({
    $where: [
      {
        equals: ["id", room]
      }
    ]
  });

  if (rooms.length < 1) return res.status(404).json({ message: "Unknown room" });

  // TODO: check access to the room
  if (!user.space_ids.includes(rooms[0].space_id)) return res.status(403).json({ message: "Forbidden" });

  const at = new AccessToken(key, secret, {
    identity: user.id,
    // token to expire after 10 minutes
    //ttl: '10m',
    ttl: '5m', // 5 minutes to match room creation
  });
  at.addGrant({
    roomJoin: true,
    room: room, // TODO: experiment with admin and join permissions
    canPublish: true, // edit for permissions later
    canSubscribe: true,
  });

  mgr.createOrIgnore(room);

  const token = at.toJwt();
  mgr.addToken(token, user.id, room, rooms[0].space_id!);

  return res.status(200).json({ token: token });
});

export default router;