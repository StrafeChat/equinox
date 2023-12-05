require('dotenv').config();
import { Client, auth } from "cassandra-driver";
import { createClient } from 'redis';
import express from "express";
import fs from "fs";
import bodyParser from "body-parser";
import cors from "cors";
import { WsHandler } from "./ws";
const app = express();

const cassandra = new Client({
    contactPoints: [process.env.CASSANDRA_CONTACT_POINT!],
    localDataCenter: process.env.CASSANDRA_DATA_CENTER,
    keyspace: process.env.CASSANDRA_KEYSPACE
})
const redis = createClient();

app.use(bodyParser.json());
app.use(cors());

try {
    (async () => {
        await redis.on('error', err => { throw new Error(err) }).connect();
        await cassandra.connect();
        const server = app.listen(process.env.PORT ?? 443, () => {
            console.log("Listening on port " + process.env.PORT!);
        });

        const versions = fs.readdirSync("src/routes");
        const types = fs.readdirSync("src/types");
        const tables = fs.readdirSync("src/tables");

        for (const version of versions) {
            const routes = fs.readdirSync(`src/routes/${version}`);
            for (const route of routes) {
                app.use(`/${version}/${route.replace(".ts", '')}`, require(`./routes/${version}/${route}`).default);
            }
        }

        for(const type of types) {
            const query = fs.readFileSync(`src/types/${type}`).toString("utf-8");
            await cassandra.execute(query);
        }

        for (const table of tables) {
            const query = fs.readFileSync(`src/tables/${table}`).toString("utf-8");
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

export { cassandra, redis };