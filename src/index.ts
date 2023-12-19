require('dotenv').config();
import { Client } from "cassandra-driver";
import express from "express";
import fs from "fs";
import http from "http";
import https from "https";
import bodyParser from "body-parser";
import cors from "cors";
import { WsHandler } from "./ws";
const app = express();

const cassandra = new Client({
    contactPoints: [process.env.SCYLLA_CONTACT_POINT1!, process.env.SCYLLA_CONTACT_POINT2!, process.env.SCYLLA_CONTACT_POINT3!],
    localDataCenter: process.env.SCYLLA_DATA_CENTER,
    credentials: { username: process.env.SCYLLA_USERNAME!, password: process.env.SCYLLA_PASSWORD! },
    keyspace: process.env.SCYLLA_KEYSPACE
})

//const redis = createClient({
//   url: process.env.REDIS_URL,
//});

app.use(bodyParser.json({ limit: "25mb" }));
app.use(cors());

try {
    (async () => {
        // await redis.on('error', err => { throw new Error(err) }).connect();
        await cassandra.connect();

        let server: https.Server | http.Server;

        if (process.env.ENV == "PROD") {
            const options = {
                key: fs.readFileSync('src/ssl/priv.pem'),
                cert: fs.readFileSync('src/ssl/pub.pem'),
            };
            server = https.createServer(options, app).listen(process.env.PORT ?? 443, function () {
                console.log("Listening on port " + process.env.PORT ?? 443);
            });
        } else {
            server = app.listen(process.env.PORT ?? 443, () => {
                console.log("Listening on port " + process.env.PORT ?? 443);
            })
        }

        const versions = fs.readdirSync("src/routes");
        const types = fs.readdirSync("src/types");
        const tables = fs.readdirSync("src/tables");
        const materialViews = fs.readdirSync("src/material_views");
        const indexes = fs.readdirSync("src/indexes");

        for (const version of versions) {
            const routes = fs.readdirSync(`src/routes/${version}`);
            for (const route of routes) {
                app.use(`/${version}/${route.replace(".ts", '')}`, require(`./routes/${version}/${route}`).default);
            }
        }

        for (const type of types) {
            const query = fs.readFileSync(`src/types/${type}`).toString("utf-8").replace("{keyspace}", cassandra.keyspace);
            await cassandra.execute(query);
        }

        for (const table of tables) {
            const query = fs.readFileSync(`src/tables/${table}`).toString("utf-8").replace("{keyspace}", cassandra.keyspace);
            await cassandra.execute(query);
        }

        for (const materialView of materialViews) {
            const query = fs.readFileSync(`src/material_views/${materialView}`).toString("utf-8").replaceAll("{keyspace}", cassandra.keyspace);
            await cassandra.execute(query);
        }

        for (const index of indexes) {
            const query = fs.readFileSync(`src/indexes/${index}`).toString("utf-8").replace("{keyspace}", cassandra.keyspace);
            await cassandra.execute(query);
        }

        new WsHandler(server);

        app.use("", (_req, res) => {
            res.status(404).json({ message: "0_o the resource you were looking for was not found!" });
        });
    })();
} catch (err) {
    console.error(err);
}

export { cassandra };
