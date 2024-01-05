import bodyParser from "body-parser";
import { Logger } from "./helpers/logger";
import cors from "cors";
import helmet from "helmet";
import express from "express";
import fs from "fs";
import {
    FRONTEND,
    NEBULA,
    PORT,
    STARGATE,
    EQUINOX,
} from './config';
import database from "./database";

//-Initialize express-//
const app = express();

app.use(bodyParser.json());
app.use(helmet());
app.use(cors({
    origin: FRONTEND,
    methods: ["POST", "PUT", "GET", "OPTIONS", "HEAD", "DELETE", "PATCH", "CONNECT", "TRACE"],
    credentials: true
}));

app.set('trust proxy', 1);
app.disable('x-powered-by');

app.use((req, res, next) => {
    res.header('Access-Control-Allow-Origin', EQUINOX);
    res.header('Access-Control-Allow-Headers', '*');
    res.header('Access-Control-Allow-Methods', '*');
    if (req.method === 'OPTIONS') {
        res.status(200).send();
    } else {
        next();
    }
});

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
        Logger.success(`Equinox is listening on ${port}!`);
    });
};

//  Start everything up
(async () => {
    Logger.start();
    await database.init();
    await startServer();
})();
