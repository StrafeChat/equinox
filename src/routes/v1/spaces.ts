import { BatchInsert } from "better-cassandra";
import { Request, Router } from "express";
import rateLimit from "express-rate-limit";
import { NEBULA, ROOM_WORKER_ID, SPACE_WORKER_ID } from "../../config";
import { cassandra } from "../../database";
import Space from "../../database/models/Space";
import { generateAcronym, generateSnowflake } from "../../helpers/generator";
import { validateSpaceCreationData, verifyToken } from "../../helpers/validator";
import { IRoom, ISpace, ISpaceMember } from "../../types";

const router = Router();

const creationRateLimit = rateLimit({
    windowMs: 24 * 60 * 60 * 1000, // 24 hours
    max: 5, // 5 spaces
    keyGenerator: (req: Request) => {
        return req.headers["authorization"]!;
    }
});

router.post('/', verifyToken, creationRateLimit, validateSpaceCreationData, async (req, res) => {
    if (res.locals.user.created_spaces_count >= 10) return res.status(403).json({ message: "You have reached the max amount of spaces you can create" });
    if (res.locals.user.space_count >= 10) return res.status(403).json({ message: "You have reached the max amount of spaces you can join" });

    const id = generateSnowflake(SPACE_WORKER_ID);
    const room_ids: string[] = [];

    for (let i = 0; i < 4; i++) {
        room_ids.push(generateSnowflake(ROOM_WORKER_ID));
    }

    const space: Partial<ISpace> = {
        id, room_ids,
        icon: null,
        name: req.body.name,
        name_acronym: generateAcronym(req.body.name, 3),
        owner_id: res.locals.user.id,
        verifcation_level: 0,
        role_ids: [],
        preferred_locale: res.locals.user.locale,
        sticker_ids: [],
        created_at: Date.now(),
        edited_at: Date.now()
    }

    console.log(space)

    await cassandra.batch([
        BatchInsert<ISpace>({
            name: "spaces", data: space
        }),
        BatchInsert<ISpaceMember>({
            name: "space_members", data: {
                user_id: res.locals.user.id,
                space_id: space.id,
                roles: [],
                joined_at: Date.now(),
                deaf: false,
                mute: false,
                avatar: null,
                edited_at: Date.now()
            }
        }),
        BatchInsert<IRoom>({
            name: "rooms", data: {
                id: room_ids[0],
                type: 0,
                space_id: id,
                position: 0,
                name: "Text Channels",
                created_at: Date.now(),
                edited_at: Date.now()
            }
        }),
        BatchInsert<IRoom>({
            name: "rooms", data: {
                id: room_ids[1],
                type: 0,
                space_id: id,
                position: 1,
                name: "Voice Channels",
                created_at: Date.now(),
                edited_at: Date.now()
            }
        }),
        BatchInsert<IRoom>({
            name: "rooms", data: {
                id: room_ids[2],
                type: 1,
                parent_id: room_ids[0],
                space_id: id,
                position: 0,
                name: "General",
                created_at: Date.now(),
                edited_at: Date.now()
            }
        }),
        BatchInsert<IRoom>({
            name: "rooms", data: {
                id: room_ids[3],
                type: 2,
                parent_id: room_ids[1],
                space_id: id,
                position: 1,
                name: "General",
                created_at: Date.now(),
                edited_at: Date.now()
            }
        })
    ], { prepare: true });

    if (space.icon) {
        await fetch(`${NEBULA}/spaces`, {
            method: "PUT",
            headers: {
                "Content-Type": "application/json",
                "Authorization": req.headers["authorization"]!
            },
            body: req.body
        }).catch(async (err) => {
            console.error(err);
            return res.status(500).json({ data: space, message: "Failed to upload icon for your space. The space will still be created but it will not have an icon." });
        }).then(async (_req) => {
            const { icon } = await _req.json();
            await Space.update({ $set: { icon }, $where: [{ equals: ['id', id] }] })
            space.icon = icon;
            return res.status(200).json({ space: space });
        });
    } else return res.status(200).json(space);
});

export default router;