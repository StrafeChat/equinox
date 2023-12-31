import { Router } from "express";
import cors from "cors";
import { JoiRegister } from "../../helpers/validator";
import { RegisterBody } from "../../types/auth";
import rateLimit from "express-rate-limit";
import { ErrorCodes } from "../../config";
import session from "express-session";
const { middleware, CaptchaGenerator } = require("@strafechat/captcha");

const router = Router();

// Set a rate limit of 5 requests every 12 hours for an ip.
// router.use(rateLimit({
//     windowMs: 12 * 60 * 60 * 1000,
//     limit: 5,
//     standardHeaders: "draft-7",
//     legacyHeaders: false,
// }));

// router.use(cors({ origin: process.env.FRONTEND_URL }));

router.get("/captcha", async (req, res) => {
    res.status(200).json({ image: await (req as any).generateCaptcha() });
})

// Route for handling register requests
router.post<{}, {}, RegisterBody>("/register", JoiRegister, async (req, res) => {
    // Express will return the error so we should try and catch to prevent that if it does happen.
    try {
        console.log((req as any).session);
        console.log((req as unknown as { verifyCaptcha: (input: string) => void }).verifyCaptcha(req.body.captcha));

        res.status(200).json({ message: "Success!" });

    } catch (err) {
        // Send back internal server error if something goes wrong.
        console.trace(err);
        res.status(ErrorCodes.INTERNAL_SERVER_ERROR.CODE).json({ message: ErrorCodes.INTERNAL_SERVER_ERROR.MESSAGE })
    }
});

export default router;