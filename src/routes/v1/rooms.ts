import { Router } from "express";
import { Validator } from "../../utility/Validator";
import { Collection } from "../../utility/Collection";
import { Generator } from "../../utility/Generator";
import { cassandra } from "../..";
import { WsHandler } from "../../ws";
import { OpCodes } from "../../ws/OpCodes";
import { User } from "../../interfaces/User";

const router = Router();

router.post("/:roomId/messages", Validator.verifyToken, async (req, res) => {
    if (!req.body.content || req.body.content.trim() == '') return res.status(400).json({ message: "At the moment, content is required for messages." });

    const room = await Collection.rooms.fetchById(req.params.roomId);
    if (!room) return res.status(404).json({ message: "The room you are trying to send a message in does not exist." });

    switch (room.type) {
        case 0:
            if (!room.recipients.includes(req.body.user.id)) return res.status(401).json({ message: "You do not have permission to send messages in this room." });
            const id = Generator.snowflake.generate();
            const currentTimestamp = Date.now();
            res.status(202).send({
                status: "Sent"
            });

            const data = {
                id,
                room_id: room.id,
                author_id: req.body.user.id,
                content: req.body.content,
                created_at: currentTimestamp,
                edited_at: currentTimestamp,
                tts: false,
                mentions: [],
                mention_roles: [],
                mention_rooms: [],
                attachments: [],
                embeds: [],
                reactions: [],
                pinned: false,
                type: 0
            };

            const keys = Object.keys(data);

            // await cassandra.batch([
            //     {
            //         query: `
            //         INSERT INTO ${cassandra.keyspace}.messages (
            //             ${keys.join(", ")}
            //         ) VALUES (${keys.map(() => '?').join(", ")});
            //         `,
            //         params: keys.map((key) => (data as any)[key])
            //     }, {
            //         query: `
            //         INSERT INTO ${cassandra.keyspace}.messages_by_room (
            //             message_id, room_id
            //         ) VALUES(?, ?);
            //         `,
            //         params: [id, req.params.roomId]
            //     }
            // ], { prepare: true });

            await cassandra.execute(`
            INSERT INTO ${cassandra.keyspace}.messages (
                ${keys.join(", ")}
            ) VALUES (${keys.map(() => '?').join(", ")});
            `, keys.map((key) => (data as any)[key]), { prepare: true });

            let author: Partial<User>;

            if (data.author_id) author = Generator.stripUserInfo(await Collection.users.fetchById(data.author_id) as User);
            else author = {};

            for (const recipient of room.recipients) {
                WsHandler.sockets.get(recipient)?.send(JSON.stringify({
                    op: OpCodes.DISPATCH,
                    event: "MESSAGE_CREATE",
                    data: { ...data, author }
                }));
            }
            break;
        default:
            return res.status(403).json({ message: "This channel does not support message sending." });
    }
});

router.get("/:roomId/messages", Validator.verifyToken, async (req, res) => {
    const messages: any[] = [];
    let after = req.query.after ?? null;
    let before = req.query.before ?? (Date.now() * 2).toString();
    let limit = parseInt(req.query.limit?.toString() ?? "30");

    if (limit > 100) return res.status(400).json({ message: "You cannot get more than 100 messages at a time." });

    const data = await cassandra.execute(`
    SELECT * FROM ${cassandra.keyspace}.messages_by_room
    WHERE room_id=? AND id${after ? '>' : '<'}?
    ORDER BY id DESC
    LIMIT ?;
  `, [req.params.roomId, after ?? before, limit], { prepare: true });

    if (data.rowLength < 1) return res.status(404).json({ message: "There are no messages in this channel" });

    for (const row of data.rows) {
        let author: Partial<User>;

        if (row.get("author_id")) author = Generator.stripUserInfo(await Collection.users.fetchById(row.get("author_id")) as User);
        else author = {};

        messages.push({ ...row, author });
    }

    res.status(200).json(messages.sort((a, b) => a.created_at.getTime() - b.created_at.getTime()));
});

export default router;
