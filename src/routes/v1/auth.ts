import { Router } from "express";
import cors from "cors";
import { JoiRegister } from "../../helpers/validator";
import { RegisterBody } from "../../types/auth";
import rateLimit from "express-rate-limit";
import { ErrorCodes } from "../../config";
import { captcha } from "../..";
const { middleware } = require("@strafechat/captcha");

const router = Router();

// Set a rate limit of 5 requests every 12 hours for an ip.
router.use(rateLimit({
    windowMs: 12 * 60 * 60 * 1000,
    limit: 5,
    standardHeaders: "draft-7",
    legacyHeaders: false,
}));

router.use(cors({ origin: process.env.FRONTEND_URL }));
router.use(middleware(captcha));

// Route for handling register requests
router.post<{}, {}, RegisterBody>("/register", JoiRegister, async (req, res) => {
    // Express will return the error so we should try and catch to prevent that if it does happen.
    try {
        console.log(req.body);
        res.status(200).json({ message: "Success!" });
        // const userByEmail = await cassandra.execute(`
        // SELECT COUNT(*)
        // FROM ${cassandra.keyspace}.users_by_email 
        // WHERE email=?
        // LIMIT 1;
        // `, [username, discriminator], { prepare: true });

        // if (userByEmail.rowLength > 0) return res.status(ErrorCodes.EMAIL_ALREADY_EXISTS.CODE).json({ message: ErrorCodes.EMAIL_ALREADY_EXISTS.MESSAGE });

        // const userByUsernameAndDiscrim = await cassandra.execute(`
        // SELECT COUNT(*)
        // FROM ${cassandra.keyspace}.users_by_username_and_discriminator 
        // WHERE username=? AND discriminator=? 
        // LIMIT 1;
        // `)

        // if (userByUsernameAndDiscrim.rowLength > 0) return res.status(ErrorCodes.USERNAME_AND_DISCRIMINATOR_TAKEN.CODE).json({ message: ErrorCodes.USERNAME_AND_DISCRIMINATOR_TAKEN.MESSAGE });

        // const id = generateSnowflake(0);
        // const verification_key = atob(id) + generateRandomString();
        // const secret = generateRandomString();
        // const last_pass_reset = Date.now();

        // const salt = process.env.PASSWORD_HASHING_SALT;

        // const hash = await bcrypt.hash(password, salt ? parseInt(salt) : 10);

        // const insertEntries = Object.entries({
        //     id, email, username, global_name, locale, discriminator, dob, secret, last_pass_reset, verification_key,
        //     password: hash,
        //     banned: false,
        //     bot: false,
        //     system: false,
        //     mfa_enabled: false,
        //     accent_color: 0xFFFFFF,
        //     verified: false,
        //     flags: 0,
        //     premium_type: 0,
        //     public_flags: 1 << 10,
        //     hide: false,
        //     presence: {
        //         status: "online",
        //         status_text: "",
        //         online: false,
        //     },
        //     created_at: Date.now(),
        //     edited_at: Date.now(),
        // });

        // const sanitizedParams = insertEntries.map(([, value]) => {
        //     return value !== undefined ? value : null;
        // });

        // const data = await resend.emails.send({
        //     from: "Strafe <no-reply@strafe.chat>",
        //     to: [email],
        //     subject: "Verify your Strafe account",
        //     html: `<h1>Welcome ${username}#${discriminator.toString().padStart(4, '0')} to Strafe!</h1> <p>Please verify your account by clicking <a href="${`${process.env.FRONTEND_URL}/verify?key=${verification_key}`}">here</a>.</p>`,
        // });

        // if (data.error) return res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE });

        // await cassandra.batch([
        //     {
        //         query: `INSERT INTO ${cassandra.keyspace}.users (
        //                     ${insertEntries.map(([key]) => key).join(',')}
        //                 ) VALUES (${insertEntries.map(() => '?').join(', ')});`,
        //         params: sanitizedParams
        //     },
        //     {
        //         query: `INSERT INTO ${cassandra.keyspace}.users_by_email (id, email) VALUES (?, ?);`,
        //         params: [id, email]
        //     },
        //     {
        //         query: `INSERT INTO ${cassandra.keyspace}.users_by_username_and_discriminator (id, username, discriminator) VALUES (?, ?, ?);`,
        //         params: [id, username, discriminator],
        //     }
        // ], { prepare: true });

        // return res.status(201).json({ message: "Waiting on email verification." });
    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.trace(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;