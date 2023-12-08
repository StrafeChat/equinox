import { Router } from "express";
import { Validator } from "../../utility/Validator";
import { cassandra } from "../..";
import { User } from "../../interfaces/User";
import { WsHandler } from "../../ws";
import { OpCodes } from "../../ws/OpCodes";
const router = Router();

router.get("/@me/relationships", Validator.verifyToken, async (req, res) => {
    try {
        const relationships = [];
        const senderQuery = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests_by_sender
        WHERE sender_id=?
        `, [req.body.user.id], { prepare: true });
        const receiverQuery = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
        WHERE receiver_id=?
        `, [req.body.user.id], { prepare: true });

        for (const row of senderQuery.rows) {
            relationships.push(row);
        }

        for (const row of receiverQuery.rows) {
            relationships.push(row);
        }

        res.status(200).json({ relationships });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" })
    }
});

router.post("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const sender: User = req.body.user;
        const query = req.params.query.split("-");

        if (sender.username == query[0] && sender.discriminator == parseInt(query[1])) return res.status(400).json({
            message: "You cannot add yourself as a friend :/",
        });

        const receiverQuery = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.users_by_username_and_discriminator
        WHERE username=?, discriminator=?
        `, [query[0], parseInt(query[1])]);

        if (receiverQuery.rowLength < 1) return res.status(404).json({
            message: "A user was not found to add as a friend.",
        });

        const existsQuery1 = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests
        WHERE sender_id=?, receiver_id=?
        `, [sender.id, receiverQuery.rows[0].get("id")]);

        const existsQuery2 = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests
        WHERE sender_id=?, receiver_id=?
        `, [receiverQuery.rows[0].get("id"), sender.id]);

        if (existsQuery1.rowLength > 0 && existsQuery2.rowLength > 0) return res.status(409).json({ message: "Friend request already sent." });

        const currentDate = new Date();

        await cassandra.batch([{
            query: `
            INSERT INTO ${cassandra.keyspace}.requests (
                sender_id, receiver_id, status, created_at
            ) VALUES(?, ?, ?);
            `,
            params: [sender.id, receiverQuery.rows[0].get("id"), "pending", currentDate]
        }, {
            query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_receiver (
                sender_id, receiver_id, status
            ) VALUES (?, ?, ?);
            `,
            params: [sender.id, receiverQuery.rows[0].get("id"), "pending"]
        }, {
            query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_sender (
                sender_id, receiver_id, status
            ) VALUES (?, ?, ?);
            `,
            params: [sender.id, receiverQuery.rows[0].get("id"), "pending"]
        }]);

        const request = { sender_id: sender.id, receiver_id: receiverQuery.rows[0].get("id"), status: "pending", created_at: currentDate }

        WsHandler.sockets.get(receiverQuery.rows[0].get("id"))?.send(JSON.stringify({ op: OpCodes.FRIEND_REQUEST, data: request }));

        return res.status(201).json(request);
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong 0_0. Try again later." });
    }
});

export default router;