require("dotenv").config();
import bodyParser from "body-parser";
import session from "express-session";
import { Client } from "cassandra-driver";
import cors from "cors";
import express from "express";
import fs from "fs";

const { CaptchaGenerator, middleware } = require("@strafechat/captcha");

// Grab constants
const {
    FRONTEND_URL,
    SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER,
    SCYLLA_USERNAME,
    SCYLLA_PASSWORD,
    SCYLLA_KEYSPACE,
    RESEND_API_KEY,
    PORT,
    SESSION_SECRET
} = process.env;

// Throw errors if important constants are missing
if (!FRONTEND_URL) throw new Error("Missing FRONTEND_URL in environment variables.");
if (!SCYLLA_CONTACT_POINTS) throw new Error("Missing an array of contact points for cassandra or scylla in the environmental variables.");
if (!SCYLLA_DATA_CENTER) throw new Error("Missing data center for cassandra or scylla in the environmental variables.");
if (!SCYLLA_KEYSPACE) throw new Error("Missing keyspace for cassandra or scylla in the environmental variables.");
if (!RESEND_API_KEY) throw new Error("Missing RESEND_API_KEY in the environmental variables.");

// Initialize express
const app = express();

// TODO: Use wildcard for cors whenever bots are added.
app.use(bodyParser.json());

app.use(cors({
    origin: FRONTEND_URL,
}));

app.use(session({
    secret: SESSION_SECRET || "equinox", // TODO: implement a better way of handling this
}));

// Initialize cassandra client
let cassandra: Client = new Client({
    contactPoints: JSON.parse(SCYLLA_CONTACT_POINTS),
    localDataCenter: SCYLLA_DATA_CENTER,
    keyspace: SCYLLA_KEYSPACE,
    credentials: (SCYLLA_USERNAME && SCYLLA_PASSWORD) ? {
        username: SCYLLA_USERNAME,
        password: SCYLLA_PASSWORD
    } : undefined
});

const captcha = new CaptchaGenerator(75, 300);

// Startup logic for equinox
const startServer = async () => {
    const port = PORT ?? 443;

    const versions = fs.readdirSync("src/routes");

    for (const version of versions) {
        const routes = fs.readdirSync(`src/routes/${version}`);
        for (const route of routes) {
            app.use(`/${version}/${route.replace(".ts", '')}`, require(`./routes/${version}/${route}`).default);
        }
    }

    app.get("/", (_req, res) => {
        res.redirect("/v1");
    });

    app.get("/v1", async (_req, res) => {
        res.status(200).json({ version: "1.0.0", release: "Early Alpha", ws: "wss://stargate.strafe.chat", file_system: "https://nebula.strafe.chat", web_application: "https://web.strafe.chat" });
    });

    // TODO: Captcha
    app.get("/captcha", cors({ origin: process.env.FRONTEND_URL }), middleware(captcha), async (req, res) => {
        res.status(200);
        res.send({ image: await (req as any).generateCaptcha() });
    });

            /*
        // verifying captchas looks like this:
        const captchaInput = req.body.captcha;
        const verified = (req as any).verifyCaptcha(capchtaInput);
        */

    app.listen(port, () => {
        console.log(`Equinox is listening on ${port}!`);
    });
};

//  Start everything up
(async () => {
    await cassandra.connect();
    await startServer();
})();

export { cassandra, captcha };
