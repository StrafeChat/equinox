import { BatchInsert } from "better-cassandra";
import { Request, Router } from "express";
import rateLimit from "express-rate-limit";
import {
  ErrorCodes,
  NEBULA,
  ROLE_WORKER_ID,
  ROOM_WORKER_ID,
  SPACE_WORKER_ID,
} from "../../config";
import { cassandra } from "../../database";
import Space from "../../database/models/Space";
import User from "../../database/models/User";
import { generateAcronym, generateSnowflake } from "../../helpers/generator";
import {
  validateRoleCreationData,
  validateSpaceCreationData,
  verifyToken,
} from "../../helpers/validator";
import { IRoom, ISpace, ISpaceMember, ISpaceRole } from "../../types";
import Room from "../../database/models/Room";
import SpaceMember from "../../database/models/SpaceMember";
import SpaceRole from "../../database/models/SpaceRole";

const router = Router();

const creationSpaceimit = rateLimit({
  windowMs: 24 * 60 * 60 * 10000, // 24 hours
  max: 5, // 5 spaces
});

const creationRoleLimit = rateLimit({
  windowMs: 24 * 60 * 60 * 10000, // 24 hours
  max: 5, // 50 roles
});

router.post(
  "/",
  verifyToken,
  creationSpaceimit,
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

      await Promise.all(
        members.map(async (member) => {
          let user = await User.select({
            $limit: 1,
            $include: [
              "username",
              "global_name",
              "avatar",
              "flags",
              "presence",
              "discriminator",
              "created_at",
              "id",
            ],
            $where: [{ equals: ["id", member.user_id] }],
          });
          (member as any).user = user[0];
          (member as any).user.display_name =
            user[0].global_name ?? user[0].username;
        })
      );

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
      res
        .status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE)
        .json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE });
    }
  }
);

router.post(
  "/:space_id/roles",
  verifyToken,
  creationRoleLimit,
  validateRoleCreationData,
  async (req, res) => {
    const { space_id } = req.params;
    const {
      name,
      color,
      hoist,
      rank,
      allowed_permissions,
      denied_permissions,
    } = req.body;

    const spaces = await Space.select({
      $where: [{ equals: ["id", space_id] }],
      $limit: 1,
    });

    const space = spaces[0];

    if (!space)
      return res
        .status(404)
        .json({ message: "The space you were looking for does not exist." });

    if (space.owner_id !== res.locals.user.id)
      // TOD: Add Manage Roles perm or space owner
      return res.status(403).json({
        message: "You do not have permission to create roles on this space.",
      });

    try {
      let id = generateSnowflake(ROLE_WORKER_ID);

      const role: ISpaceRole = {
        id,
        name,
        hoist,
        color,
        rank,
        allowed_permissions,
        denied_permissions,
        icon: null,
        space_id: space.id!,
        edited_at: null,
        created_at: Date.now(),
      };

      await SpaceRole.insert(role, {
        prepare: true,
      });

      res.status(200).json({
        role,
      });
    } catch (error) {
      console.error(error);
      res
        .status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE)
        .json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE });
    }
  }
);

export default router;