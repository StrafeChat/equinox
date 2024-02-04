import bodyParser from "body-parser";
import cors from "cors";
import express from "express";
import rateLimit from "express-rate-limit";
import fs from "fs";
import {
    FRONTEND,
    NEBULA,
    PORT,
    STARGATE,
} from './config';
import database from "./database";
import { Logger } from "./helpers/logger";

//-Initialize express-//
const app = express();

app.use(bodyParser.json());
app.use(cors());

app.set('trust proxy', 1);
app.disable('x-powered-by');

app.use((req, res, next) => {
  res.header('Access-Control-Allow-Origin', '*');
  res.header('Access-Control-Allow-Methods', 'GET, POST, PUT, DELETE');
    if (req.method === 'OPTIONS') {
        res.status(200).send();
    } else {
        next();
    }
});

// app.use(rateLimit({
//     windowMs: 3 * 60 * 60 * 1000,
//     limit: 100,
//     standardHeaders: "draft-7",
//     legacyHeaders: false
// }));

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
