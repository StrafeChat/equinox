import bodyParser from "body-parser";
import cors from "cors";
import express from "express";
import rateLimit from "express-rate-limit";
import fs from "fs";
import { FRONTEND, NEBULA, PORT, STARGATE } from './config';
import database from "./database";
import { Logger } from "./helpers/logger";
import path from "path";

//-Initialize express-//
const app = express();

app.use(bodyParser.json({ limit: "25mb" }));

app.use(cors({
    origin: [FRONTEND, "http://localhost:3001"],
    methods: ['GET', 'POST', 'PUT', 'PATCH', 'DELETE'], 
    credentials: true,
}));

app.set('trust proxy', 1);
app.disable('x-powered-by');

// const limiter = rateLimit({
//     windowMs: 10 * 1000,
//     max: 75,
//   });

// CORS preflight handling
app.options('*', cors());

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

    app.get("/worker.js", (_req, res) => res.sendFile(path.join(__dirname, "/static/worker.js")));

    app.use(express.static(path.join(__dirname, "/static")));

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
