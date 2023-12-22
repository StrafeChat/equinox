import { Router } from "express";
import Validator from "../../utility/Validator";
import staff from "../../resources/staff.json";
import { cassandra } from "../..";
import bcrypt from "bcrypt";
import Generator from "../../utility/Generator";
import WsHandler from "../../stargate";
import { OpCodes } from "../../stargate/OpCodes";

const router = Router();

router.post("/auth", async (req, res) => {
    try {
        const { error } = Validator.login(req.body);
        if (error) return res.status(401).json({ message: "Incorrect Password" });

        const exists = await cassandra.execute(`
        SELECT id, email
        FROM ${cassandra.keyspace}.users_by_email 
        WHERE email=?
        LIMIT 1;
        `, [req.body.email], { prepare: true });

        if (exists.rowLength < 1) return res.status(404).json({ message: "User does not exist with this email." });
        if (!staff.includes(exists.rows[0].get("id"))) return res.status(401).json({ message: "Unauthorized" });

        const user = await cassandra.execute(`
        SELECT id, password, secret
        FROM ${cassandra.keyspace}.users
        WHERE id=?
        LIMIT 1;
        `, [exists.rows[0].get("id")], { prepare: true });

        if (user.rowLength < 1) return res.status(500).json({ message: "Something went very wrong when trying to login, please contact strafe support!" });

        const validPass = await bcrypt.compare(req.body.password, user.rows[0].get("password"));
        if (!validPass) return res.status(401).json({ message: "The password you have entered is incorrect." });
        const token = Generator.token(user.rows[0].get("id"), Date.now(), user.rows[0].get("secret"));
        return res.status(200).json({ token });
    } catch (err) {
        console.log(err);
        return res.status(500).json({ message: "Internal Server Error" });
    }
});

router.get("/@me", Validator.verifyToken, (req, res) => {
    if (!staff.includes(req.user!.id)) return res.status(401).json({ message: "Not Authorized" });
    res.status(200).json({ ...req.user });
});

router.get("/users/:id", Validator.verifyToken, async (req, res) => {
    if (isNaN(parseInt(req.params.id))) return res.status(400).json({ message: "The id you have provided is not correct!" });

    const data = await cassandra.execute(`
    SELECT avatar, email, username, discriminator, flags, id FROM ${cassandra.keyspace}.users
    WHERE id=?
    LIMIT 1;
    `, [req.params.id]);

    if (data.rowLength < 1) return res.status(404).json({ message: `Could not find a user by that id.` });

    // const toReturn = Generator.stripSpecific<User>(data.rows[0] as unknown as User, ["hide", "last_pass_reset", "mfa_enabled", "password", "premium_type", "secret"])

    return res.status(200).json(data.rows[0]);
})

router.patch("/users/:userId", Validator.verifyToken, async (req, res) => {
    try {
        if (!staff.includes(req.user!.id)) return res.status(401).json({ message: "Not Authorized" });

        const user = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.users
        WHERE id=?
        LIMIT 1;
        `, [req.params.userId], { prepare: true });

        if (user.rowLength < 1) return res.status(404).json({ message: "User not found" });

        await cassandra.execute(`
        UPDATE ${cassandra.keyspace}.users
        SET email=?, username=?, discriminator=?, flags=?
        WHERE id=? AND created_at=?;
        `, [req.body.email, req.body.username, parseInt(req.body.discriminator), req.body.flags, req.params.userId, user.rows[0].get("created_at")], { prepare: true });

        const sendUpdateToSocket = async (requestRows: any[], senderOrReceiver: any) => {
            for (const row of requestRows) {
                const socketId = row.get(`${senderOrReceiver}_id`);
                const socket = WsHandler.sockets.get(socketId);
                if (socket) {
                    socket.send(JSON.stringify({ op: OpCodes.DISPATCH, data: { ...req.body }, event: "USER_UPDATE" }));
                }
            }
        };

        const receiverRequests = await cassandra.execute(
            `
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=?
            LIMIT 1;`,
            [req.params.userId], {}
        );

        await sendUpdateToSocket(receiverRequests.rows, "sender");

        const senderRequests = await cassandra.execute(
            `
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=?
            LIMIT 1;
            `,
            [req.params.userId]
        );

        await sendUpdateToSocket(senderRequests.rows, "receiver");

        res.status(200).json({ message: "Success!" });
    } catch (err) {
        console.log(err);
        return res.status(500).json({ message: "Internal Server Error" });
    }
});

export default router;