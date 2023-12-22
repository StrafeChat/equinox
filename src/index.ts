require('dotenv').config();
import { Client } from "cassandra-driver";
import express from "express";
import fs from "fs";
import http from "http";
import https from "https";
import bodyParser from "body-parser";
import cors from "cors";
import WsHandler from "./stargate";
import rateLimit from "express-rate-limit";
import Database from "./utility/Database";

const { SCYLLA_CONTACT_POINT1, SCYLLA_CONTACT_POINT2, SCYLLA_CONTACT_POINT3, SCYLLA_DATA_CENTER, SCYLLA_USERNAME, SCYLLA_PASSWORD, SCYLLA_KEYSPACE, ENV, PORT, WEBSOCKET_PORT } = process.env;

const app = express();
let cassandra!: Client;

const limiter = rateLimit({
	windowMs: 60 * 1000, 
	limit: 250, 
	standardHeaders: 'draft-7',
	legacyHeaders: false,
})

app.use(bodyParser.json({ limit: "25mb" }));
app.use(cors());
app.use(limiter);

let server: https.Server | http.Server;
let wsServer: https.Server | http.Server;

const sslOptions = {
    key: fs.readFileSync('src/ssl/priv.pem'),
    cert: fs.readFileSync('src/ssl/pub.pem'),
};

const startServer = async ({ secure }: { secure: boolean }) => {
    try {
        if (secure) {
            cassandra = new Client({
                contactPoints: [SCYLLA_CONTACT_POINT1!],
                localDataCenter: SCYLLA_DATA_CENTER,
                credentials: { username: SCYLLA_USERNAME!, password: SCYLLA_PASSWORD! },
                keyspace: SCYLLA_KEYSPACE
            });

            await cassandra.connect();

            server = http.createServer(app).listen(PORT ?? 443, () => {
                console.log("Equinox Listening on port " + PORT ?? 443);
            });

            wsServer = https.createServer(sslOptions).listen(WEBSOCKET_PORT ?? 8080, () => {
                console.log("Stargate Listening on port " + WEBSOCKET_PORT ?? 8080);
            });
        } else {
            cassandra = new Client({
                contactPoints: [SCYLLA_CONTACT_POINT1!, SCYLLA_CONTACT_POINT2!, SCYLLA_CONTACT_POINT3!],
                localDataCenter: SCYLLA_DATA_CENTER,
                credentials: { username: SCYLLA_USERNAME!, password: SCYLLA_PASSWORD! },
                keyspace: SCYLLA_KEYSPACE
            });

            await cassandra.connect();

            server = app.listen(PORT ?? 443, () => {
                console.log("Equinox on port " + PORT ?? 443);
            });
            
            wsServer = http.createServer().listen(WEBSOCKET_PORT ?? 8080, () => {
                console.log("Stargate Listening on port " + WEBSOCKET_PORT ?? 8080);
            });
        }
    } catch (err) {
        console.log('Error during server startup:', err);
        if (server) server.close();
        if (wsServer) wsServer.close();
    }
}

try {
    (async () => {
        if (ENV == "PROD") await startServer({ secure: true })
        else await startServer({ secure: false });

        const versions = fs.readdirSync("src/routes");

        for (const version of versions) {
            const routes = fs.readdirSync(`src/routes/${version}`);
            for (const route of routes) {
                app.use(`/${version}/${route.replace(".ts", '')}`, require(`./routes/${version}/${route}`).default);
            }
        }

        Database.init();

        console.log(wsServer!);

        new WsHandler(wsServer!);

        app.get("/", (_req, res) => {
            res.redirect("/v1");
        });
        app.get("/v1", async (_req, res) => {
            res.status(200).json({ version: "1.0.0", release: "Early Alpha", ws: "wss://stargate.strafe.chat", file_system: "https://nebula.strafe.chat", web_application: "https://web.strafe.chat" });
        });
        app.use("", (_req, res) => {
            res.status(404).json({ message: "0_o the resource you were looking for was not found!" });
        });
    })();
} catch (err) {
    console.error(err);
}

process.on("unhandledRejection", (reason, promise) =>
    console.log(`Unhandled Rejection at: ${promise} reason: ${reason}`)
);
process.on("uncaughtException", (err) =>
    console.log(`Uncaught Exception: ${err}`)
);

export { cassandra };
