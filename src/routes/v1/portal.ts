import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import Room from "../../database/models/Room";
import { AccessToken } from "livekit-server-sdk";

const router = Router();

router.post("/rooms/:roomId/join", verifyToken, async (req, res) => {
  const room = req.params.roomId;
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
  if (!user.space_ids.includes(rooms[0].space_id!)) return res.status(403).json({ message: "Forbidden" });

  const at = new AccessToken(process.env.LIVEKIT_API_KEY, process.env.LIVEKIT_API_SECRET, {
    identity: user.global_name ?? user.username,
    ttl: '10m',
  });
  at.addGrant({ roomJoin: true, room: room });

  return res.status(200).json({ token: at.toJwt() });
});

export default router;