import { BatchInsert } from "better-cassandra";
import { Request, Router } from "express";
import rateLimit from "express-rate-limit";
import { ErrorCodes, NEBULA, ROOM_WORKER_ID, SPACE_WORKER_ID } from "../../config";
import { cassandra } from "../../database";
import Space from "../../database/models/Space";
import User from "../../database/models/User";
import { generateAcronym, generateSnowflake } from "../../helpers/generator";
import {
  validateSpaceCreationData,
  verifyToken,
} from "../../helpers/validator";
import { IRoom, ISpace, ISpaceMember } from "../../types";
import Room from "../../database/models/Room";
import SpaceMember from "../../database/models/SpaceMember";

const router = Router();

const creationRateLimit = rateLimit({
  windowMs: 24 * 60 * 60 * 10000, // 24 hours
  max: 5, // 5 spaces
});

router.post(
  "/",
  verifyToken,
  creationRateLimit,
  validateSpaceCreationData,
  async (req, res) => {

    try {
    if (res.locals.user.created_spaces_count! >= 10)
      return res.status(403).json({
        message: "You have reached the max amount of spaces you can create.",
      });
    if (res.locals.user.space_count! >= 50)
      return res.status(403).json({
        message: "You have reached the max amount of spaces you can join.",
      });

    const id = generateSnowflake(SPACE_WORKER_ID);
    const room_ids: string[] = [];

    for (let i = 0; i < 4; i++) {
      room_ids.push(generateSnowflake(ROOM_WORKER_ID));
    }

    const space = {
      id,
      room_ids,
      icon: null,
      name: req.body.name,
      name_acronym: generateAcronym(req.body.name, 3),
      owner_id: res.locals.user.id,
      verifcation_level: 0,
      role_ids: [],
      preferred_locale: res.locals.user.locale,
      sticker_ids: [],
      created_at: Date.now(),
    };

    await cassandra.batch(
      [
        BatchInsert<ISpace>({
          name: "spaces",
          data: space,
        }),
        BatchInsert<ISpaceMember>({
          name: "space_members",
          data: {
            user_id: res.locals.user.id,
            space_id: space.id,
            roles: [],
            joined_at: Date.now(),
            deaf: false,
            mute: false,
            avatar: null,
            edited_at: Date.now(),
          },
        }),
        BatchInsert<IRoom>({
          name: "rooms",
          data: {
            id: room_ids[0],
            type: 0,
            space_id: id,
            position: 0,
            name: "Text Rooms",
            created_at: Date.now(),
            edited_at: Date.now(),
          },
        }),
        BatchInsert<IRoom>({
          name: "rooms",
          data: {
            id: room_ids[1],
            type: 0,
            space_id: id,
            position: 1,
            name: "Voice Rooms",
            created_at: Date.now(),
            edited_at: Date.now(),
          },
        }),
        BatchInsert<IRoom>({
          name: "rooms",
          data: {
            id: room_ids[2],
            type: 1,
            parent_id: room_ids[0],
            space_id: id,
            position: 0,
            name: "General",
            created_at: Date.now(),
            edited_at: Date.now(),
          },
        }),
        BatchInsert<IRoom>({
          name: "rooms",
          data: {
            id: room_ids[3],
            type: 2,
            parent_id: room_ids[1],
            space_id: id,
            position: 1,
            name: "General",
            created_at: Date.now(),
            edited_at: Date.now(),
          },
        }),
      ],
      { prepare: true }
    );

    let space_ids = res.locals.user.space_ids ?? [];
    space_ids.push(`${space.id}`);
    await User.update({
      $set: { space_ids },
      $where: [{ equals: ["id", res.locals.user.id] }],
    });

    const roomIds = room_ids.map((id) => id.toString());
    const rooms = await Room.select({
      $where: [{ in: ["id", roomIds] }],
    });

      let members = await SpaceMember.select({
        $where: [{ equals: ["space_id", space.id] }],
      });

      await Promise.all(members.map(async (member) => {
        let user = await User.select({
          $limit: 1,
          $include: ["username", "global_name", "avatar", "flags", "presence", "discriminator", "created_at", "id"],
          $where: [{ equals: ["id", member.user_id] }],
        });
           (member as any).user = user[0];
           (member as any).user.display_name = user[0].global_name ?? user[0].username;
       }));

       (space as any).members = members;

    for (const room of rooms) {
      (room as any).messages = [];
    }

    if (space.icon) {
      await fetch(`${NEBULA}/spaces`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Authorization: req.headers["authorization"]!,
        },
        body: req.body,
      })
        .catch(async (err) => {
          console.error(err);
          return res.status(500).json({
            data: space,
            message:
              "Failed to upload icon for your space. The space will still be created but it will not have an icon.",
          });
        })
        .then(async (_req) => {
          const { icon } = await _req.json();
          await Space.update({
            $set: { icon },
            $where: [{ equals: ["id", id] }],
          });
          space.icon = icon;
          return res.status(200).json({ space, rooms });
        });
    } else return res.status(200).json({ space, rooms });
    
  } catch (err) {
    console.error("Space creation failed:", err);
    res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
}
  }
);

// router.post(
//   "/:space_id/rooms/:room_id/messages",
//   verifyToken,
//   messageCreateLimit,
//   async (req, res) => {
//     const { space_id, room_id } = req.params;

//     if (isNaN(parseInt(space_id)))
//       return res.status(400).json({ message: "Invalid space id" });
//     if (isNaN(parseInt(room_id)))
//       return res.status(400).json({ message: "Invalid room id" });

//     const { content } = req.body;

//     if (typeof content !== "string")
//       return res
//         .status(400)
//         .json({ message: "Message content must be a string." });
//     if (content.length > 1024)
//       return res.status(400).json({
//         message: "Message content must be less than 1,024 characters long.",
//       });
//     const rooms = await Room.select({
//       $where: [{ equals: ["id", room_id] }],
//       $limit: 1,
//     });
//     if (!rooms[0])
//       return res.status(404).json("A room was not found with the provided id.");

//     if (rooms[0].space_id !== space_id)
//       return res
//         .status(400)
//         .json({ message: "This room is not in this space." });

//     const members = await SpaceMember.selectAll({
//       $where: [{ equals: ["space_id", space_id] }],
//     });
    
//     res.status(201).json({message: })

//     for (const member of members) {
        
//     }
//   }
// );

export default router;
