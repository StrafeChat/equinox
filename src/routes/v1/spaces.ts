import { BatchInsert } from "better-cassandra";
import { Router } from "express";
import rateLimit from "express-rate-limit";
import { NEBULA, SPACE_WORKER_ID } from "../../config";
import { cassandra, redis } from "../../database";
import Space from "../../database/models/Space";
import { generateAcronym, generateSnowflake } from "../../helpers/generator";
import { verifyToken } from "../../helpers/validator";
import { IRoom, ISpace } from "../../types";

const router = Router();

const creationRateLimit = rateLimit({
    windowMs: 24 * 60 * 60 * 1000, // 24 hours
    max: 5,
});

const publishSpace = async (data: Partial<ISpace>) => {
    await redis.publish("stargate", JSON.stringify({
        event: "SPACE_CREATED",
        data
    }));
}

router.post('/', creationRateLimit, verifyToken, async (req, res) => {
    if (res.locals.user.created_spaces_count >= 10) return res.status(403).json({ message: "You have reached the max amount of spaces you can create" });
    if (res.locals.user.space_count >= 10) return res.status(403).json({ message: "You have reached the max amount of spaces you can join" });

    const id = generateSnowflake(SPACE_WORKER_ID);
    const room_ids: string[] = [];

    for (let i = 0; i < 4; i++) {
        room_ids.push(generateSnowflake(SPACE_WORKER_ID));
    }

    const space: Partial<ISpace> = {
        id, room_ids,
        icon: null,
        name: req.body.name,
        // Can you make it take the 3 letters from the name and use that or use 1/2 letters depending on the amount of words
        nameAcronym: generateAcronym(req.body.name, 3),
        owner_id: res.locals.user.id,
        verifcation_level: 0,
        role_ids: [],
        preferred_locale: res.locals.user.locale,
        sticker_ids: [],
        created_at: Date.now(),
        edited_at: Date.now()
    }

    await cassandra.batch([
        BatchInsert<ISpace>({
            name: "spaces", data: space
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
                space_id: id,
                position: 1,
                name: "General",
                created_at: Date.now(),
                edited_at: Date.now()
            }
        })
    ]);

    await fetch(`${NEBULA}/spaces`, {
        method: "POST",
        headers: {
            "Content-Type": "application/json",
            "Authorization": req.headers["authorization"]!
        },
        body: req.body
    }).catch(async (err) => {
        console.error(err);
        await publishSpace(space);
        return res.status(500).json({ data: space, message: "Failed to upload icon for your space. The space will still be created but it will not have an icon." });
    }).then(async (_req) => {
        const { icon } = await _req.json();
        await Space.update({ $set: { icon }, $where: [{ equals: ['id', id] }] })
        space.icon = icon;
        await publishSpace(space);
        return res.status(200).json(space);
    });
});

export default router;