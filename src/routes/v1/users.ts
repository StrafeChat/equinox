import { Request, Response, Router } from "express";
import { Validator } from "../../utility/Validator";
import { cassandra } from "../..";
import { User } from "../../interfaces/User";
import { WsHandler } from "../../ws";
import { OpCodes } from "../../ws/OpCodes";
import { Generator } from "../../utility/Generator";
import { Collection } from "../../utility/Collection";
import { Relationship } from "../../interfaces/Request";
const router = Router();

router.get("/@me/relationships", Validator.verifyToken, async (req, res) => {
    try {
        const relationships: Relationship[] = [];

        const sent = await Collection.requests.fetchManySenderRequests(req.body.user.id);
        const received = await Collection.requests.fetchManyReceiverRequests(req.body.user.id);

        await Promise.all(sent.map(async (relationship) => {
            relationship.sender = (relationship.sender_id === req.body.user.id ? Generator.stripUserInfo(req.body.user) : await Collection.users.fetchById(relationship.receiver_id));
            relationship.receiver = await Collection.users.fetchById(relationship.receiver_id);
            relationships.push(relationship);
        }));

        await Promise.all(received.map(async (relationship) => {
            relationship.sender = await Collection.users.fetchById(relationship.sender_id);
            relationship.receiver = (relationship.receiver_id === req.body.user.id ? Generator.stripUserInfo(req.body.user) : await Collection.users.fetchById(relationship.sender_id));
            relationships.push(relationship);
        }));

        res.status(200).json({ relationships });
    } catch (err) {
        console.log(err);
        res.status(500).json({ message: "Something went wrong on our side 0_0, try again maybe?" });
    }
});

const handleAccept = async (req: Request, res: Response, query: string[]) => {
    const receiver = req.body.user;
    const sender = await Collection.users.fetchByUsernameAndDiscrim(query[0], parseInt(query[1]));

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

const handleReject = async (req: Request, res: Response, query: string[]) => {
    const userFromQuery = await Collection.users.fetchByUsernameAndDiscrim(query[0], parseInt(query[1]));

    if (!userFromQuery) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

    let request = await Collection.requests.fetchReceiverRequest(req.body.user.id, userFromQuery.id);

    if (!request) request = await Collection.requests.fetchReceiverRequest(userFromQuery.id, req.body.user.id);

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

    let sender = req.body.user;
    if (request.sender_id != req.body.user.id) {
        sender = await Collection.users.fetchById(userFromQuery.id);
    }

    let receiver = req.body.user;
    if (request.receiver_id != req.body.user.id) {
        receiver = Collection.users.fetchById(userFromQuery.id);
    }

    const relationship = { sender_id: request.sender_id, sender: Generator.stripUserInfo(sender), receiver_id: request.receiver_id, receiver: Generator.stripUserInfo(receiver), status: "rejected", created_at: request.created_at };

    WsHandler.sockets.get(userFromQuery.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

    return res.status(200).json({ relationship });
}

const handleDelete = async (req: Request, res: Response, query: string[]) => {
    const userFromQuery = await Collection.users.fetchByUsernameAndDiscrim(query[0], parseInt(query[1]));

    if (!userFromQuery) return res.status(404).json({ message: "Oops, looks like this user does not exist anymore. Maybe they were banned?" });

    let request = await Collection.requests.fetchReceiverRequest(req.body.user.id, userFromQuery.id);

    if (!request) request = await Collection.requests.fetchReceiverRequest(userFromQuery.id, req.body.user.id);

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

    let sender = req.body.user;
    if (request.sender_id != req.body.user.id) {
        sender = await Collection.users.fetchById(userFromQuery.id);
    }

    let receiver = req.body.user;
    if (request.receiver_id != req.body.user.id) {
        receiver = await Collection.users.fetchById(userFromQuery.id);
    }

    const relationship = { sender_id: request.sender_id, sender: Generator.stripUserInfo(sender), receiver_id: request.receiver_id, receiver: Generator.stripUserInfo(receiver), status: "deleted", created_at: request.created_at };

    WsHandler.sockets.get(userFromQuery.id)?.send(JSON.stringify({ op: OpCodes.RELATIONSHIP_UPDATE, data: relationship }));

    return res.status(200).json({ relationship });
}

router.patch("/@me/relationships/:query", Validator.verifyToken, async (req, res) => {
    try {
        const query = req.params.query.split('-');
        const { action } = req.body;

        if (query.length < 2 || query.length > 2) return res.status(401).json({ message: "Invalid query." });
        if (isNaN(parseInt(query[1]))) return res.status(401).json({ message: "Discriminator must be a number." });

        switch (action) {
            case "accept":
                return await handleAccept(req, res, query);
            case "reject":
                return await handleReject(req, res, query);
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


export default router;