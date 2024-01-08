import { CaptchaGenerator, middleware } from "@strafechat/captcha";
import bcrypt from "bcrypt";
import { BatchInsert } from "better-cassandra";
import crypto from "crypto";
import { Router } from "express";
import rateLimit from "express-rate-limit";
import session from "express-session";
import { Resend } from "resend";
import { ErrorCodes, FRONTEND, PASSWORD_HASHING_SALT } from "../../config";
import { cassandra } from "../../database";
import User from "../../database/models/User";
import UserByEmail from "../../database/models/UserByEmail";
import UserByUsernameAndDiscriminator from "../../database/models/UserByUsernameAndDiscriminator";
import Verification from "../../database/models/Verification";
import { generateRandomString, generateSnowflake, generateToken } from "../../helpers/generator";
import { JoiRegister, verifyToken } from "../../helpers/validator";
import { IUser, IUserByEmail, IUserByUsernameAndDiscriminator, IVerification, LoginBody, RegisterBody } from "../../types";
const resend = new Resend(process.env.RESEND_API_KEY!);

const router = Router();

const captcha = new CaptchaGenerator(75, 600);

router.use(rateLimit({
    windowMs: 3 * 60 * 60 * 1000,
    limit: 5,
    standardHeaders: "draft-7",
    legacyHeaders: false
}));

router.use(session({
    resave: false,
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
        const verifyId = Buffer.from(id).toString("base64url");
        const verifyCode = `${crypto.randomInt(1, 999999)}`.padStart(6, '0');

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
            }),
            BatchInsert<IVerification>({
                name: "verifications",
                data: {
                    id: verifyId,
                    code: verifyCode
                }
            })
        ], { prepare: true });

        await resend.emails.send({
            from: "Strafe <no-reply@strafe.chat>",
            to: [email],
            subject: "Verify your Strafe account",
            html: `<html lang="en" style="overflow:hidden"><meta content="width=100%,initial-scale=1" name="viewport"><body style="color:#fff;font-family:Arial"><div style="background:#232629;border-radius:8px;padding:5px;width:100%;display:inline-block"><a href="https://strafe.chat" style="text-decoration:none;color:#fff"><center><center><img src="https://res.cloudinary.com/sup/image/upload/v1704245175/avatars/c5kpdx4pgkreomdlppta.png" style="background-color:#0000004d;border-radius:50%;width:80px;margin-top:20px;margin-bottom:10px"><h1 style="font-size:24px">Strafe</h1></center></center><p style="text-align:center;padding:0;font-size:18px;margin:10px 0 20px 0">Your confirmation Code:<center><code style="background-color:#f1f1f1;color:#333;padding:2px 4px;border-radius:4px;font-family:'Courier New',monospace">${verifyCode}</code></center><p style="text-align:center;margin:30px 10px 20px 10px;opacity:.8;font-size:14px"><a style="color:#22c55e" href="${FRONTEND}/verify/${verifyId}?code=${verifyCode}">Click here</a>to verify your email.<br><br>If you did not request this code, you can safely ignore this email.<br>Sent by<a style="color:#fff" href="https://strafe.chat">StrafeChat</a></div></body></html>`
        });

        return res.status(201).json({ message: "Waiting on email verification." })
    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.log(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

router.post<string, { id: string }, {}, { code: string }, {}, { user: IUser }>("/verify", verifyToken, async (req, res) => {
    try {
        if (typeof req.params.id != "string") return res.status(400).json({ message: "You need to specify an id to proceed with verification." });
        if (typeof req.body.code != "string") return res.status(400).json({ message: "You need to specify a verification code to proceed with verification." });

        if (res.locals.user.id != atob(req.params.id)) return res.status(401).json({ message: "You can only verify yourself." });

        const verifications = await Verification.select({
            $where: [{
                equals: ["code", req.body.code]
            }, {
                equals: ["id", req.params.id]
            }]
        });

        if (verifications!.length < 1) return res.status(404).json({ message: "You are already verified!" });

        // TODO: Delete row and send token back to user and message.
    } catch (err) {
        console.error(err);
        return res.status(500).json({ message: "Internal Server Error" });
    }
});

router.post<{}, {}, LoginBody>("/login", async (req, res) => {
    try {
        const { email, captcha } = req.body;

        const result = (req as unknown as { verifyCaptcha: (input: string) => boolean }).verifyCaptcha(captcha);
        if (!result) return res.status(400).json({ message: "Invalid captcha" });

        req.session.destroy((err) => {
            if (err) console.log(err);
        });

        const existsEmails = await UserByEmail.select({ $where: [{ equals: ["email", email] }] });
        if (existsEmails?.length! < 1) return res.status(409).json({ message: "A user does not exist with this email." });

        const users = await User.select({ $include: ["id", "last_pass_reset", "secret", "password"], $where: [{ equals: ["id", existsEmails![0].id] }, { equals: ["created_at", existsEmails![0].created_at] }] });

        const { id, last_pass_reset, secret, password } = users![0];

        const validPass = await bcrypt.compare(req.body.password, password!);
        if (!validPass) return res.status(401).json({ message: "Invalid Password" });

        res.status(200).json({ token: generateToken(id!, last_pass_reset!, secret!) });
    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.log(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;