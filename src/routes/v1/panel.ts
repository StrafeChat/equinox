import { Router } from "express";
import { Validator } from "../../utility/Validator";
import staff from "../../resources/staff.json";
import { cassandra } from "../..";
import bcrypt from "bcrypt";
import { Generator } from "../../utility/Generator";

const router = Router();

router.post("/auth", async (req, res) => {
    try {
        console.log(req.body);
        const { error } = Validator.login(req.body);
        console.log(error);
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
    if (!staff.includes(req.body.user.id)) return res.status(401).json({ message: "Not Authorized" });
    res.status(200).json({ ...req.body.user });
});

export default router;