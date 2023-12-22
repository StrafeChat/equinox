import { Request, Response, Router } from "express";
import Validator from "../../utility/Validator";
import { cassandra } from "../..";
import WsHandler from "../../stargate";
import OpCodes from "../../stargate/OpCodes";
import Generator from "../../utility/Generator";
import Collection from "../../utility/Collection";
import Relationship from "../../interfaces/Request";
import User from "../../interfaces/User";
const router = Router();

router.get("/@me/relationships", Validator.verifyToken, async (req, res) => {
    try {
        const relationships: Relationship[] = [];

        const sent = await Collection.requests.fetchManySenderRequests(req.user!.id);
        const received = await Collection.requests.fetchManyReceiverRequests(req.user!.id);

        await Promise.all(sent.map(async (relationship) => {
            relationship.sender = (relationship.sender_id === req.user!.id ? Generator.stripUserInfo(req.user!) : await Collection.users.fetchById(relationship.receiver_id));
            relationship.receiver = await Collection.users.fetchById(relationship.receiver_id);
            relationships.push(relationship);
        }));

        await Promise.all(received.map(async (relationship) => {
            relationship.sender = await Collection.users.fetchById(relationship.sender_id);
            relationship.receiver = (relationship.receiver_id === req.user!.id ? Generator.stripUserInfo(req.user!) : await Collection.users.fetchById(relationship.sender_id));
            relationships.push(relationship);
        }));

        res.status(200).json({ relationships });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

const handleAccept = async (req: Request, res: Response, id: string) => {
    const receiver = req.user!;
    const sender = await Collection.users.fetchById(id);

    if (!sender) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

    const request = await Collection.requests.fetchSenderRequest(sender.id, receiver.id);

    if (!request || request.status != "pending") return res.status(404).json({ message: "Friend request not found." });

    await cassandra.batch([
        {
            query: `
            UPDATE ${cassandra.keyspace}.requests_by_sender
            SET status=?
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: ["accepted", sender.id, receiver.id, request.created_at]
        },
        {
            query: `
            UPDATE ${cassandra.keyspace}.requests_by_receiver
            SET status=?
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: ["accepted", sender.id, receiver.id, request.created_at]
        }
    ], { prepare: true });

    const relationship = { sender_id: sender.id, sender: Generator.stripUserInfo(sender), receiver_id: receiver.id, receiver: Generator.stripUserInfo(receiver), status: "accepted", created_at: request.created_at };

    WsHandler.sockets.get(sender.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

    return res.status(200).json({ relationship });
}

const handleReject = async (req: Request, res: Response, id: string) => {
    const user = await Collection.users.fetchById(id);

    if (!user) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

    let request = await Collection.requests.fetchReceiverRequest(req.user!.id, user.id);

    if (!request) request = await Collection.requests.fetchReceiverRequest(user.id, req.user!.id);

    if (!request || request.status != "pending") return res.status(404).json({ message: "Friend request not found." });

    await cassandra.batch([
        {
            query: `
            DELETE FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: [request.sender_id, request.receiver_id, request.created_at]
        },
        {
            query: `
            DELETE FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: [request.sender_id, request.receiver_id, request.created_at]
        }
    ], { prepare: true });

    let sender = req.user!;
    if (request.sender_id != req.user!.id) {
        sender = user;
    }

    let receiver = req.user!;
    if (request.receiver_id != req.user!.id) {
        receiver = user;
    }

    const relationship = { sender_id: request.sender_id, sender: Generator.stripUserInfo(sender), receiver_id: request.receiver_id, receiver: Generator.stripUserInfo(receiver), status: "rejected", created_at: request.created_at };

    WsHandler.sockets.get(user.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

    return res.status(200).json({ relationship });
}

const handleDelete = async (req: Request, res: Response, query: string[]) => {
    const userFromQuery = await Collection.users.fetchByUsernameAndDiscrim(query[0], parseInt(query[1]));

    if (!userFromQuery) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

    let request = await Collection.requests.fetchReceiverRequest(req.user!.id, userFromQuery.id);

    if (!request) request = await Collection.requests.fetchReceiverRequest(userFromQuery.id, req.user!.id);

    if (!request) return res.status(404).json({ message: "Friend request not found." });

    await cassandra.batch([
        {
            query: `
            DELETE FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: [request.sender_id, request.receiver_id, request.created_at]
        },
        {
            query: `
            DELETE FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE sender_id=? AND receiver_id=? AND created_at=?
            `,
            params: [request.sender_id, request.receiver_id, request.created_at]
        }
    ], { prepare: true });

    let sender = req.user!;
    if (request.sender_id != req.user!.id) {
        sender = (await Collection.users.fetchById(userFromQuery.id))!;
    }

    let receiver: User = req.user!;

    if (request.receiver_id != req.user!.id) {
        receiver = (await Collection.users.fetchById(userFromQuery.id))!;
    }

    const relationship = { sender_id: request.sender_id, sender: Generator.stripUserInfo(sender), receiver_id: request.receiver_id, receiver: Generator.stripUserInfo(receiver), status: "deleted", created_at: request.created_at };

    WsHandler.sockets.get(userFromQuery.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

    return res.status(200).json({ relationship });
}

router.patch("/@me/relationships/:userId", Validator.verifyToken, async (req, res) => {
    try {
        const { action } = req.body;

        if (isNaN(parseInt(req.params.userId))) return res.status(401).json({ message: "The user id does not look right." });

        switch (action) {
            case "accept":
                return await handleAccept(req, res, req.params.userId);
            case "reject":
                return await handleReject(req, res, req.params.userId);
        }
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side... You should try again maybe?" });
    }
});

router.post("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const sender = req.user!;
        const query = req.params.query.split("-");

        if (query.length < 2 || query.length > 2) return res.status(401).json({ message: "Invalid query." });
        if (isNaN(parseInt(query[1]))) return res.status(401).json({ message: "Discriminator must be a number." });

        if (sender.username == query[0] && sender.discriminator == parseInt(query[1])) return res.status(400).json({
            message: "You cannot add yourself as a friend :/",
        });

        const receiver = await Collection.users.fetchByUsernameAndDiscrim(query[0], parseInt(query[1]));

        if (!receiver) return res.status(404).json({
            message: "A user was not found to add as a friend.",
        });

        const existsBySender = await Collection.requests.fetchSenderRequest(sender.id, receiver.id);
        const existsByReceiver = await Collection.requests.fetchSenderRequest(receiver.id, sender.id);

        if (existsBySender || existsByReceiver) return res.status(409).json({ message: "Friend request already sent." });

        const currentDate = new Date();

        await cassandra.batch([
            {
                query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_sender (
                sender_id, receiver_id, status, created_at
            ) VALUES (?, ?, ?, ?); 
            `,
                params: [sender.id, receiver.id, "pending", currentDate],
            }, {
                query: `
            INSERT INTO ${cassandra.keyspace}.requests_by_receiver (
                sender_id, receiver_id, status, created_at
            ) VALUES (?, ?, ?, ?); 
            `,
                params: [sender.id, receiver.id, "pending", currentDate],
            }
        ], { prepare: true })

        const relationship = { sender_id: sender.id, sender: sender, receiver_id: receiver.id, receiver: receiver, status: "pending", created_at: currentDate };

        WsHandler.sockets.get(receiver.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

        return res.status(201).json({ relationship });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong 0_0. Try again later." });
    }
});

router.get("/@me", Validator.verifyToken, (req, res) => {
    res.status(200).json({ user: req.user! });
});

router.patch("/@me", Validator.verifyToken, async (req, res) => {
    try {
        const { username, discriminator, avatar, global_name } = req.body;
        if (!username && !avatar && !discriminator && !global_name) return res.status(401).json({ message: "You must specify what you are trying to update." });

        if (avatar) {
            const token = Generator.token(req.user!.id, req.user!.last_pass_reset.getTime(), req.user!.secret);
            const response = await fetch(`${process.env.CDN}/avatars`, {
                method: "PATCH",
                headers: {
                    "Content-Type": "application/json",
                    "Authorization": token
                },
                body: JSON.stringify({
                    data: avatar.replace(/^data:image\/(png|jpeg|jpg|gif|webp);base64,/, '')
                })
            });

            if (!response.ok) return res.status(400).json({ message: "Something went wrong." });

            const data = await response.json();

            req.user!.avatar = data.hash;
        }

        if (username || discriminator) {
            console.log(global_name);
            const usernameToInsert = username ?? req.user.username;
            const discriminatorToInsert = isNaN(parseInt(`${discriminator}`)) ? req.user.discriminator : parseInt(`${discriminator}`);

            if (username != req.user.username || discriminator != req.user.discriminator) {
                const update = [usernameToInsert, discriminatorToInsert];

                if (global_name && global_name != req.user.global_name) update.push(global_name);
                update.push(req.user!.id, req.user!.created_at);

                await cassandra.batch([
                    {
                        query: `
                        UPDATE ${cassandra.keyspace}.users
                        SET username=?, discriminator=?${global_name ? (global_name != req.user.global_name ? ", global_name=?" : '') : ''}
                        WHERE id=? AND created_at=?;
                        `,
                        params: update
                    },
                    {
                        query: `
                        DELETE FROM ${cassandra.keyspace}.users_by_username_and_discriminator
                        WHERE username=? AND discriminator=?;
                        `,
                        params: [req.user!.username, req.user!.discriminator]
                    },
                    {
                        query: `
                            INSERT INTO ${cassandra.keyspace}.users_by_username_and_discriminator (
                                username, discriminator, id
                            ) VALUES (?, ?, ?);
                        `,
                        params: [usernameToInsert, discriminatorToInsert, req.user.id]
                    }
                ], { prepare: true });
                req.user.username = usernameToInsert;
                req.user.discriminator = discriminatorToInsert;
            }
        } else {
            console.log(global_name);
            if (global_name && req.user.global_name != global_name) {
                await cassandra.execute(`
                UPDATE ${cassandra.keyspace}.users
                SET global_name=?
                WHERE id=? AND created_at=?;
                `, [global_name, req.user.id, req.user.created_at]);
                req.user.global_name = global_name;
            }
        }

        await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
        WHERE receiver_id=?;
        `, [req.user!.id], { prepare: true }).then((request) => {
            for (const row of request.rows) {
                WsHandler.sockets.get(row.get("sender_id"))?.send(JSON.stringify({ op: OpCodes.DISPATCH, data: { ...req.user }, event: "USER_UPDATE" }))
            }
        });

        await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.requests_by_sender
        WHERE sender_id=?;
        `, [req.user!.id], { prepare: true }).then((request) => {
            for (const row of request.rows) {
                WsHandler.sockets.get(row.get("receiver_id"))?.send(JSON.stringify({ op: OpCodes.DISPATCH, data: { ...req.user }, event: "USER_UPDATE" }))
            }
        });

        return res.status(200).json({ ...req.user });
    } catch (err) {
        console.log(err);
        return res.status(500).json({ message: "Oops" });
    }
});

router.delete("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const query = req.params.query.split('-');
        if (query.length < 2 || query.length > 2) return res.status(401).json({ message: "Invalid query." });
        if (isNaN(parseInt(query[1]))) return res.status(401).json({ message: "Discriminator must be a number." });
        await handleDelete(req, res, query);
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side... You should try again maybe?" });
    }
});

router.post("/@me/rooms", Validator.verifyToken, async (req, res) => {
    try {
        if (!req.body.recipientId) return res.status(400).json({ message: "You need to specify a recipient id when creating a pm." });
        if (req.body.type == null || req.body.type == undefined || isNaN(parseInt(req.body.type))) return res.status(400).json({ message: "You must specify a type for the room." });
        if (req.body.recipientId == req.user!.id) return res.status(400).json({ message: "You cannot create a pm for only you :/" });

        switch (req.body.type) {
            // pm room
            case 0:
                const execution = await cassandra.execute(`
                SELECT * FROM ${cassandra.keyspace}.room_recipients_by_user
                WHERE recipients CONTAINS ? AND recipients CONTAINS ?
                LIMIT 1 ALLOW FILTERING;
                `, [req.user!.id, req.body.recipientId], { prepare: true });

                if (execution.rowLength > 0) return res.status(401).json({ message: "You already have a pm with this user." });

                const id = Generator.snowflake.generate();

                const recipients = [req.user!.id, req.body.recipientId];

                await cassandra.batch([
                    {
                        query: `
                        INSERT INTO ${cassandra.keyspace}.rooms (
                            id, recipients, total_messages_sent, type, created_at, edited_at
                        ) VALUES (?, ?, ?, ?, ?, ?); 
                        `,
                        params: [id, recipients, 0, 0, Date.now(), Date.now()]
                    },
                    ...recipients.map((recipient) => {
                        return {
                            query: `
                            INSERT INTO ${cassandra.keyspace}.room_recipients_by_user (
                                room_id, user_id, recipients, created_at
                            ) VALUES(?, ?, ?, ?)
                            `,
                            params: [id, recipient, recipients, Date.now()]
                        };
                    }),
                ], { prepare: true });
                break;
            // group room
            case 1:
                break;
            default:
                return res.status(401).json({ message: "You can only create a group or pm room on this route." });
        }
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side... You should try again maybe?" });
    }
});

router.get("/@me/rooms", Validator.verifyToken, async (req, res) => {
    const rooms = await Collection.rooms.fetchManyByUserId(req.user!.id);
    res.status(200).json({ rooms });
});

export default router;