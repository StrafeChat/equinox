import { Router } from "express";
import { ErrorCodes } from "../../config";
import { generateSnowflake } from "../../helpers/generator";
import { JoiRegister } from "../../helpers/validator";
import { RegisterBody } from "../../types/auth";
const { middleware, CaptchaGenerator } = require("@strafechat/captcha");

const router = Router();

const captcha = new CaptchaGenerator(75, 600);

router.use(middleware(captcha));

router.get("/captcha", async (req, res) => {
    req.sessionID = generateSnowflake(0);
    res.status(200).json({ image: await (req as any).generateCaptcha(), sessionId: req.sessionID });
    console.log((req as any).sessions);
})

// Route for handling register requests
router.post<{}, {}, RegisterBody>("/register", JoiRegister, async (req, res) => {
    // Express will return the error so we should try and catch to prevent that if it does happen.
    try {
        const result = (req as unknown as { verifyCaptcha: (input: string) => boolean }).verifyCaptcha(req.body.captcha);
        if (!result) return res.status(400).json({ message: "Invalid captcha" });
        res.status(200).json({ status: result });

    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.trace(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;