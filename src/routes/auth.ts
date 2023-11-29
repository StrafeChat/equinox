import { Router } from "express";
import { Register, Validator } from "../utility/Validator";
import { client } from "..";
import { Generator } from "../utility/Generator";
import bcrypt from "bcrypt";

const router = Router();

router.post("/register", async (req, res) => {
    const { error } = Validator.register(req.body);
    if (error) return res.status(401).json(error.details[0]);

    const { email, global_name, username, discriminator, password, dob, locale } = req.body as Register;

    try {
        const existsByUsernameAndDiscriminator = await client.execute(`
        SELECT id, username, discriminator 
        FROM ${client.keyspace}.users_by_username_and_discriminator 
        WHERE username=? AND discriminator=? 
        LIMIT 1;
        `, [username, discriminator], { prepare: true });

        const existsByEmail = await client.execute(`
        SELECT id, email
        FROM ${client.keyspace}.users_by_email 
        WHERE email=?
        LIMIT 1;
        `, [email]);

        if (existsByUsernameAndDiscriminator.rowLength > 0) return res.status(403).json({ message: "A user already exists with this username and discriminator." });
        if (existsByEmail.rowLength > 0) return res.status(403).json({ message: "A user already exists with this email." });

        const id = Generator.snowflake.generate();
        const secret = Generator.randomKey();
        const last_pass_reset = Date.now();

        const hashedPass = await bcrypt.hash(password, parseInt(process.env.PASSWORD_HASHING_SALT!));

        const insertEntries = Object.entries({
            id, email, username, global_name, locale, discriminator, dob, secret, last_pass_reset,
            password: hashedPass,
            bot: false,
            system: false,
            mfa_enabled: false,
            accent_color: null,
            verified: false,
            flags: 0,
            premium_type: 0,
            public_flags: 0,
            hide: false,
            created_at: Date.now(),
            edited_at: Date.now()
        });

        const sanitizedParams = insertEntries.map(([, value]) => {
            return value !== undefined ? value : null;
        });

        await client.batch([
            {
                query: `INSERT INTO ${client.keyspace}.users (
                            ${insertEntries.map(([key]) => key).join(',')}
                        ) VALUES (${insertEntries.map(() => '?').join(', ')});`,
                params: sanitizedParams
            },
            {
                query: `INSERT INTO ${client.keyspace}.users_by_email (id, email) VALUES (?, ?);`,
                params: [id, email]
            },
            {
                query: `INSERT INTO ${client.keyspace}.users_by_username_and_discriminator (id, username, discriminator) VALUES (?, ?, ?);`,
                params: [id, username, discriminator],
            }
        ], { prepare: true });

        const token = Generator.token(id, last_pass_reset, secret);
        return res.status(201).json({ token });
    } catch (err) {
        console.error(err);
        return res.status(500).json({ message: "Internal Server Error" });
    }
});

export default router;