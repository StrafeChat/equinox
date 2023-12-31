import { CaptchaGenerator, middleware } from "@strafechat/captcha";
import RedisStore from "connect-redis";
import { Router } from "express";
import session from "express-session";
import { redis } from "../..";
import { ErrorCodes } from "../../config";
import { JoiRegister } from "../../helpers/validator";
import { RegisterBody } from "../../types";
import rateLimit from "express-rate-limit";

const router = Router();

const captcha = new CaptchaGenerator(75, 600);

const store = new RedisStore({
    client: redis,
    prefix: "sess:",
});

router.use(rateLimit({
    windowMs: 3 * 60 * 60 * 1000,
    limit: 5,
    standardHeaders: "draft-7",
    legacyHeaders: false
}));

router.use(session({
    store,
    saveUninitialized: true,
    secret: "StrafeChat",
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
        const result = (req as unknown as { verifyCaptcha: (input: string) => boolean }).verifyCaptcha(req.body.captcha);
        if (!result) return res.status(400).json({ message: "Invalid captcha" });

        req.session.destroy((err) => {
            if (err) console.log(err);
        });
        
        res.status(200).json({ status: result });

    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.trace(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;