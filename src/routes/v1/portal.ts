import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import { JoinBody } from "../../types/";
import Room from "../../database/models/Room";
import User from "../../database/models/User";

import { AccessToken } from 'livekit-server-sdk';
import { RoomManager, SignalingServer } from "../../portal";
import { generateToken } from "../../helpers/generator";

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

function extractIds(roomId: string) {
  return roomId.split(":");
}

router.post<{}, {}, JoinBody>("/personal/join", async (req, res) => {
  const roomId = req.body.roomId;
  const user = res.locals.user;

  const util = ((req as any).portal as { manager: RoomManager, signaling: SignalingServer });

  const users = extractIds(roomId);

  const checkUser = (await User.select({
    $where: [{ equals: ["id", users[0]] }],
    $limit: 1
  }))[0];

  if (!checkUser) return res.status(404).send({ message: "Unkown user. (" + users[0] + ")" });

  if (!checkUser.friends?.includes(users[1])) return res.status(403).send({ message: "You need to be friends with " + users[1] + " to initiate a call." })

  const token = util.signaling.tokens.grantToken(user.id, roomId);

  return res.status(200).json({ token: token });
});

export default router;