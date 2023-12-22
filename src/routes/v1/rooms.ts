import { Router } from "express";
import Validator from "../../utility/Validator";
import Collection from "../../utility/Collection";
import Generator from "../../utility/Generator";
import { cassandra } from "../..";
import WsHandler from "../../stargate";
import OpCodes from "../../stargate/OpCodes";
import User from "../../interfaces/User";

const router = Router();

router.get("/:roomId/messages", Validator.verifyToken, async (req, res) => {
    try {
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
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

router.post("/:roomId/messages", Validator.verifyToken, async (req, res) => {
    try {
        const { content, reference_id } = req.body;
        if (!content || content.trim() == '') return res.status(400).json({ message: "At the moment, content is required for messages." });

        const room = await Collection.rooms.fetchById(req.params.roomId);
        if (!room) return res.status(404).json({ message: "The room you are trying to send a message in does not exist." });

        switch (room.type) {
            case 0:
                if (!room.recipients.includes(req.user!.id)) return res.status(401).json({ message: "You do not have permission to send messages in this room." });
                const id = Generator.snowflake.generate();
                const currentTimestamp = Date.now();
                res.status(202).send({
                    status: "Sent"
                });

                if (reference_id) {
                    const result = await cassandra.execute(`
                    SELECT * FROM ${cassandra.keyspace}.messages_by_room
                    WHERE id=? AND room_id=?
                    LIMIT 1;
                    `, [reference_id, req.params.roomId]);

                    if (result.rowLength < 1) return res.status(404).json({ message: "The message you were trying to reply to does not exist in this room." });
                }

                const data = {
                    id,
                    room_id: room.id,
                    author_id: req.user!.id,
                    message_reference_id: reference_id ?? null,
                    content: req.body.content,
                    created_at: currentTimestamp,
                    edited_at: null,
                    tts: false,
                    mentions: [],
                    mention_roles: [],
                    mention_rooms: [],
                    attachments: [],
                    embeds: [],
                    reactions: [],
                    pinned: false,
                    type: 0,
                };

                const keys = Object.keys(data);

                // await cassandra.execute(`
                // INSERT INTO ${cassandra.keyspace}.messages (
                //     ${keys.join(", ")}
                // ) VALUES (${keys.map(() => '?').join(", ")});
                // `, keys.map((key) => (data as any)[key]), { prepare: true });

                await cassandra.batch([
                    {
                        query: `
                        INSERT INTO ${cassandra.keyspace}.messages (
                            ${keys.join(", ")}
                        ) VALUES(${keys.map(() => '?').join(", ")});
                        `,
                        params: keys.map((key) => (data as any)[key])
                    },
                    {
                        query: `
                        UPDATE ${cassandra.keyspace}.rooms
                        SET last_message_id=?
                        WHERE id=? AND created_at=?;
                        `,
                        params: [data.id, data.room_id, room.created_at]
                    }
                ], { prepare: true });

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
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

router.patch("/:roomId/messages/:messageId", Validator.verifyToken, async (req, res) => {
    try {
        const messageQuery = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.messages_by_room
        WHERE room_id=? AND id=?
        LIMIT 1;
        `, [req.params.roomId, req.params.messageId]);

        if (messageQuery.rowLength < 1 || messageQuery.rows[0].get("room_id") != req.params.roomId) return res.status(404).json({ message: "The message you were trying to edit does not exist in this room." });
        if (messageQuery.rows[0].get("author_id") != req.user!.id) return res.status(401).json({ message: "You are not the author of the message you are trying to edit!" });

        const editedAt = new Date();

        await cassandra.execute(`
        UPDATE ${cassandra.keyspace}.messages
        SET content=?, edited_at=?
        WHERE id=? AND created_at=?
        `, [req.body.content, editedAt, req.params.messageId, messageQuery.rows[0].get("created_at")], { prepare: true });

        res.status(202).send({
            status: "Sent"
        });

        const room = await Collection.rooms.fetchById(req.params.roomId)!;

        let author: Partial<User>;

        if (messageQuery.rows[0].get("author_id")) author = Generator.stripUserInfo(await Collection.users.fetchById(messageQuery.rows[0].get("author_id")) as User);
        else author = {};

        const data = {
            ...messageQuery.rows[0],
            content: req.body.content ?? messageQuery.rows[0].get("content"),
            edited_at: editedAt
        }

        if (!room) return;

        for (const recipient of room.recipients) {
            WsHandler.sockets.get(recipient)?.send(JSON.stringify({
                op: OpCodes.DISPATCH,
                event: "MESSAGE_UPDATE",
                data: { ...data, author }
            }));
        }
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

router.delete("/:roomId/messages/:messageId", Validator.verifyToken, async (req, res) => {
    try {
        const messageQuery = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.messages_by_room
        WHERE room_id=? AND id=?
        LIMIT 1;
        `, [req.params.roomId, req.params.messageId]);

        if (messageQuery.rowLength < 1 || messageQuery.rows[0].get("room_id") != req.params.roomId) return res.status(404).json({ message: "The message you were trying to delete does not exist in this room." });
        if (messageQuery.rows[0].get("author_id") != req.user!.id) return res.status(401).json({ message: "You are not the author of the message you are trying to delete!" });

        await cassandra.execute(`
        DELETE FROM ${cassandra.keyspace}.messages
        WHERE id=? AND created_at=?;
        `, [req.params.messageId, messageQuery.rows[0].get("created_at")]);

        res.status(202).send({
            status: "Sent"
        });

        const room = await Collection.rooms.fetchById(req.params.roomId)!;

        if (!room) return;

        for (const recipient of room.recipients) {
            WsHandler.sockets.get(recipient)?.send(JSON.stringify({
                op: OpCodes.DISPATCH,
                event: "MESSAGE_DELETE",
                data: { message_id: req.params.messageId, room_id: req.params.roomId }
            }));
        }
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

router.put("/:roomId/messages/:messageId/reactions/:emoji", Validator.verifyToken, async (req, res) => {
    const { roomId, messageId, emoji } = req.params;
    return res.status(501).json({ message: "This route is not implemented yet." });
});

export default router;
