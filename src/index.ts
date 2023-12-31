import bodyParser from "body-parser";
import cors from "cors";
import express from "express";
import fs from "fs";
import {
    FRONTEND_URL,
    PORT
} from './config';
import database from "./database";

// Initialize express
const app = express();

app.use(bodyParser.json());

app.use(cors({
    origin: FRONTEND_URL,
    methods: ["POST", "PUT", "GET", "OPTIONS", "HEAD"],
    credentials: true
}));

app.set('trust proxy', 1);

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

    app.listen(port, () => {
        console.log(`Equinox is listening on ${port}!`);
    });
};

//  Start everything up
(async () => {
    await database.init();
    await startServer();
})();
