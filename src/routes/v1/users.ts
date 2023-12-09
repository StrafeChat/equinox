import { Router } from "express";
import { Validator } from "../../utility/Validator";
import { cassandra } from "../..";
import { User } from "../../interfaces/User";
import { WsHandler } from "../../ws";
import { OpCodes } from "../../ws/OpCodes";
import { Generator } from "../../utility/Generator";
const router = Router();

router.get("/@me/relationships", Validator.verifyToken, async (req, res) => {
    try {
        const relationships: any[] = [];

        const senderQuery = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=?
        `, [req.body.user.id], { prepare: true });

        const receiverQuery = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=?
        `, [req.body.user.id], { prepare: true });

        const fetchUserInfo = async (userId: string) => {
            const user = await cassandra.execute(`
                SELECT * FROM ${cassandra.keyspace}.users
                WHERE id=? 
                LIMIT 1;
            `, [userId]);
            return Generator.stripUserInfo(user.rows[0] as unknown as User);
        };

        await Promise.all(senderQuery.rows.map(async (row) => {
            row.sender = row.get("sender_id") === req.body.user.id
                ? Generator.stripUserInfo(req.body.user)
                : await fetchUserInfo(row.get("receiver_id"));

            row.receiver = await fetchUserInfo(row.get("receiver_id"));

            relationships.push(row);
        }));

        await Promise.all(receiverQuery.rows.map(async (row) => {
            row.sender = await fetchUserInfo(row.get("sender_id"));

            row.receiver = row.get("receiver_id") === req.body.user.id
                ? Generator.stripUserInfo(req.body.user)
                : await fetchUserInfo(row.get("sender_id"));

            relationships.push(row);
        }));

        res.status(200).json({ relationships });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

router.patch("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const query = req.params.query.split('-');
        const { action } = req.body;

        if (query.length < 2 || query.length > 2) return res.status(401).json({ message: "Invalid query." });
        if (isNaN(parseInt(query[1]))) return res.status(401).json({ message: "Discriminator must be a number." });

        switch (action) {
            case "accept":
                const acceptReceiver = req.body.user;
                const senderQuery = await cassandra.execute(`
                SELECT * 
                FROM ${cassandra.keyspace}.users_by_username_and_discriminator
                WHERE username=? AND discriminator=?
                LIMIT 1;
                `, [query[0], parseInt(query[1])], { prepare: true });

                if (senderQuery.rowLength < 1) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

                const acceptRequest = await cassandra.execute(`
                SELECT *
                FROM ${cassandra.keyspace}.requests_by_sender
                WHERE sender_id=? AND receiver_id=?
                LIMIT 1;
                `, [senderQuery.rows[0].get("id"), acceptReceiver.id]);

                if (!acceptRequest.rows.some((row) => row.status == "pending")) return res.status(404).json({ message: "Friend request not found." });

                await cassandra.batch([
                    {
                        query: `
                        UPDATE ${cassandra.keyspace}.requests_by_sender
                        SET status=?
                        WHERE sender_id=? AND receiver_id=? AND created_at=?
                        `,
                        params: ["accepted", senderQuery.rows[0].id, acceptReceiver.id, acceptRequest.rows[0].get("created_at")]
                    },
                    {
                        query: `
                        UPDATE ${cassandra.keyspace}.requests_by_receiver
                        SET status=?
                        WHERE sender_id=? AND receiver_id=? AND created_at=?
                        `,
                        params: ["accepted", senderQuery.rows[0].id, acceptReceiver.id, acceptRequest.rows[0].get("created_at")]
                    }
                ], { prepare: true });

                const acceptSender = await cassandra.execute(`
                SELECT * FROM ${cassandra.keyspace}.users
                WHERE id=?
                LIMIT 1;
                `, [senderQuery.rows[0].get("id")], { prepare: true });

                var relationship = { sender_id: senderQuery.rows[0].get("id"), sender: Generator.stripUserInfo(acceptSender.rows[0] as unknown as User), receiver_id: acceptReceiver.id, receiver: Generator.stripUserInfo(acceptReceiver), status: "accepted", created_at: acceptRequest.rows[0].get("created_at") };

                WsHandler.sockets.get(senderQuery.rows[0].get("id"))?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

                return res.status(200).json({ relationship });
            case "reject":
                const rejectSender = req.body.user as any;
                const receiverQuery = await cassandra.execute(`
                        SELECT * 
                        FROM ${cassandra.keyspace}.users_by_username_and_discriminator
                        WHERE username=? AND discriminator=?
                        LIMIT 1;
                    `, [query[0], parseInt(query[1])], { prepare: true });

                if (receiverQuery.rowLength < 1) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

                const rejectRequest = await cassandra.execute(`
                        SELECT *
                        FROM ${cassandra.keyspace}.requests_by_receiver
                        WHERE sender_id=? AND receiver_id=?
                        LIMIT 1;
                    `, [rejectSender.id, receiverQuery.rows[0].get("id")]);

                if (!rejectRequest.rows.some((row) => row.status == "pending")) return res.status(404).json({ message: "Friend request not found." });

                await cassandra.batch([
                    {
                        query: `
                        DELETE FROM ${cassandra.keyspace}.requests_by_sender
                        WHERE sender_id=? AND receiver_id=? AND created_at=?
                        `,
                        params: [rejectSender.id, receiverQuery.rows[0].get("id"), rejectRequest.rows[0].get("created_at")]
                    },
                    {
                        query: `
                        DELETE FROM ${cassandra.keyspace}.requests_by_receiver
                        WHERE sender_id=? AND receiver_id=? AND created_at=?
                        `,
                        params: [rejectSender.id, receiverQuery.rows[0].get("id"), rejectRequest.rows[0].get("created_at")]
                    }
                ], { prepare: true });

                relationship = { sender_id: (rejectSender as unknown as User).id, sender: Generator.stripUserInfo(rejectSender as unknown as User), receiver_id: receiverQuery.rows[0].get("id"), receiver: Generator.stripUserInfo(receiverQuery.rows[0] as unknown as User), status: "rejected", created_at: rejectRequest.rows[0].get("created_at") };

                WsHandler.sockets.get(receiverQuery.rows[0].get("id"))?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

                return res.status(200).json({ relationship });
        }
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side... You should try again maybe?" });
    }
});

router.post("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const sender: User = req.body.user;
        const query = req.params.query.split("-");

        if (query.length < 2 || query.length > 2) return res.status(401).json({ message: "Invalid query." });
        if (isNaN(parseInt(query[1]))) return res.status(401).json({ message: "Discriminator must be a number." });

        if (sender.username == query[0] && sender.discriminator == parseInt(query[1])) return res.status(400).json({
            message: "You cannot add yourself as a friend :/",
        });

        const receiverQuery = await cassandra.execute(`
        SELECT * 
        FROM ${cassandra.keyspace}.users_by_username_and_discriminator
        WHERE username=? AND discriminator=?
        LIMIT 1;
        `, [query[0], parseInt(query[1])], { prepare: true });

        if (receiverQuery.rowLength < 1) return res.status(404).json({
            message: "A user was not found to add as a friend.",
        });

        const existsQuery1 = await cassandra.execute(`
        SELECT * 
        FROM ${cassandra.keyspace}.requests_by_sender
        WHERE receiver_id=? AND sender_id=?
        LIMIT 1;
        `, [sender.id, receiverQuery.rows[0].get("id")], { prepare: true });

        const existsQuery2 = await cassandra.execute(`
        SELECT * 
        FROM ${cassandra.keyspace}.requests_by_receiver
        WHERE receiver_id=? AND sender_id=?
        LIMIT 1;
        `, [receiverQuery.rows[0].get("id"), sender.id]);

        console.log(existsQuery1.rows, existsQuery2.rows);

        if (existsQuery1.rowLength > 0 || existsQuery2.rowLength > 0) return res.status(409).json({ message: "Friend request already sent." });

        const currentDate = new Date();

        await cassandra.batch([
            {
                query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_sender (
                sender_id, receiver_id, status, created_at
            ) VALUES (?, ?, ?, ?); 
            `,
                params: [sender.id, receiverQuery.rows[0].get("id"), "pending", currentDate],
            }, {
                query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_receiver (
                sender_id, receiver_id, status, created_at
            ) VALUES (?, ?, ?, ?); 
            `,
                params: [sender.id, receiverQuery.rows[0].get("id"), "pending", currentDate],
            }
        ], { prepare: true })


        const receiver = await cassandra.execute(`
        SELECT *
        FROM ${cassandra.keyspace}.users
        WHERE id=?
        LIMIT 1;
        `, [receiverQuery.rows[0].get("id")], { prepare: true });

        const relationship = { sender_id: sender.id, sender: sender, receiver_id: receiverQuery.rows[0].get("id"), receiver: receiver.rows[0], status: "pending", created_at: currentDate };

        WsHandler.sockets.get(receiverQuery.rows[0].get("id"))?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

        return res.status(201).json({ relationship });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong 0_0. Try again later." });
    }
});

export default router;