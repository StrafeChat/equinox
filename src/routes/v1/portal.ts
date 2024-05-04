import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import { JoinBody } from "../../types/";
import Room from "../../database/models/Room";

import { AccessToken } from 'livekit-server-sdk';

const router = Router();
router.use(verifyToken);

router.post<{}, {}, JoinBody>("/join", async (req, res) => {
  const room = req.body.roomId;
  const user = res.locals.user;
  
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

  const at = new AccessToken(process.env.LIVEKIT_API_KEY, process.env.LIVEKIT_API_SECRET, {
    identity: user.id,
    // token to expire after 10 minutes
    ttl: '10m',
  });
  at.addGrant({ roomJoin: true, room: room });

  return res.status(200).json({ token: await at.toJwt() });
});

export default router;