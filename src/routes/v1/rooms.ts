import { Router } from "express";
import { Validator } from "../../utility/Validator";
import { Collection } from "../../utility/Collection";
import { Generator } from "../../utility/Generator";
import { cassandra } from "../..";

const router = Router();

router.post(`:roomId/messages`, Validator.verifyToken, async (req, res) => {
    if (!req.body.content) return res.status(400).json({ message: "At the moment, content is required for messages." });
    const room = await Collection.rooms.fetchById(req.params.id);
    if (!room) return res.status(404).json({ message: "The room you are trying to send a message in does not exist." });
    if (!room.recipients.includes(req.body.user.id)) return res.status(401).json({ message: "You are not in this room." });

    const id = Generator.snowflake.generate();

    await cassandra.execute(`
    
    `)
});

export default router;