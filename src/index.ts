import bodyParser from "body-parser";
import cors from "cors";
import express from "express";
import fs from "fs";
import {
    FRONTEND,
    NEBULA,
    PORT,
    STARGATE
} from './config';
import database from "./database";

// Initialize express
const app = express();

app.use(bodyParser.json());

app.use(cors({
    origin: FRONTEND,
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

    // Redirect to newest info route
    app.get("/", (_req, res) => {
        res.redirect("/v1/gateway");
    });

    // Send info about strafe
    app.get("/v1/gateway", async (_req, res) => {
        res.status(200).json({ version: "1.0.0", release: "Early Alpha", ws: STARGATE, file_system: NEBULA, web_application: FRONTEND });
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
