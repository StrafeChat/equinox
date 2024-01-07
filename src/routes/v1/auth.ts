import { CaptchaGenerator, middleware } from "@strafechat/captcha";
import bcrypt from "bcrypt";
import { BatchInsert } from "better-cassandra";
import RedisStore from "connect-redis";
import { Router } from "express";
import rateLimit from "express-rate-limit";
import session from "express-session";
import { ErrorCodes, PASSWORD_HASHING_SALT } from "../../config";
import { cassandra, redis } from "../../database";
import UserByEmail from "../../database/models/UserByEmail";
import UserByUsernameAndDiscriminator from "../../database/models/UserByUsernameAndDiscriminator";
import { generateRandomString, generateSnowflake, generateToken } from "../../helpers/generator";
import { JoiRegister } from "../../helpers/validator";
import { IUser, IUserByEmail, IUserByUsernameAndDiscriminator, RegisterBody } from "../../types";

const router = Router();

const captcha = new CaptchaGenerator(75, 600);

// const store = new RedisStore({
//     client: redis,
//     prefix: "sess:",
// });

router.use(rateLimit({
    windowMs: 3 * 60 * 60 * 1000,
    limit: 5,
    standardHeaders: "draft-7",
    legacyHeaders: false
}));

router.use(session({
    //store,
    resave: true,
    saveUninitialized: true,
    secret: process.env.SESSION_SECRET ?? "StrafeChat",
}));

const mw = middleware(captcha)

router.use("/", (req, res, next) => mw(req, res, next));

router.get("/captcha", async (req, res) => {
    res.status(200).json({ image: await (req as any).generateCaptcha() });
})

// Route for handling register requests
router.post<{}, {}, RegisterBody>("/register", JoiRegister, async (req, res) => {
    // Express will return the error so we should try and catch to prevent that if it does happen.
    try {
        const { email, global_name, username, discriminator, password, dob, locale, captcha } = req.body;

        const result = (req as unknown as { verifyCaptcha: (input: string) => boolean }).verifyCaptcha(captcha);
        if (!result) return res.status(400).json({ message: "Invalid captcha" });

        req.session.destroy((err) => {
            if (err) console.log(err);
        });

        const existsEmail = await UserByEmail.count({ $where: [{ equals: ["email", email] }] });
        if (existsEmail! > 0) return res.status(409).json({ message: "A user already exists with this email." });

        const existsUD = await UserByUsernameAndDiscriminator.count({ $where: [{ equals: ["username", username] }, { equals: ["discriminator", discriminator] }], $prepare: true });
        if (existsUD! > 0) return res.status(409).json({ message: "A user already exists with this username and discriminator." });

        const hashedPass = await bcrypt.hash(password, PASSWORD_HASHING_SALT);
        const created_at = Date.now();
        const id = generateSnowflake(0);
        const secret = generateRandomString(12);

        await cassandra.batch([
            BatchInsert<IUser>({
                name: "users",
                data: {
                    created_at, discriminator, dob, email, global_name, id, locale, secret, username,
                    "banned": false,
                    "bot": false,
                    "edited_at": created_at,
                    "flags": 0,
                    "last_pass_reset": created_at,
                    "mfa_enabled": false,
                    "password": hashedPass,
                    "premium_type": 0,
                    "presence": {
                        "online": false,
                        "status": "offline",
                        "status_text": ""
                    },
                    "public_flags": 0,
                    "system": false,
                    "verified": false
                }
            }),
            BatchInsert<IUserByEmail>({
                name: "users_by_email",
                data: {
                    created_at, email, id
                }
            }),
            BatchInsert<IUserByUsernameAndDiscriminator>({
                name: "users_by_username_and_discriminator",
                data: {
                    created_at, discriminator, username, id
                }
            })
        ], { prepare: true });

        res.status(200).json({ token: generateToken(id, created_at, secret) });

    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.log(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;