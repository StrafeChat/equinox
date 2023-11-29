require('dotenv').config();
import { Client } from "cassandra-driver";
import { createClient } from 'redis';
import express from "express";
import fs from "fs";
import bodyParser from "body-parser";
const app = express();

const client = new Client({
    contactPoints: [process.env.CASSANDRA_CONTACT_POINT!],
    localDataCenter: process.env.CASSANDRA_DATA_CENTER,
    keyspace: process.env.CASSANDRA_KEYSPACE
})

const redisClient = createClient();

app.use(bodyParser.json());

try {
    (async () => {
        await redisClient.on('error', err => { throw new Error(err) }).connect();
        await client.connect();
        app.listen(process.env.PORT ?? 443, () => {
            console.log("Listening on port " + process.env.PORT!);
        });

        const routes = fs.readdirSync("src/routes");
        const tables = fs.readdirSync("src/tables");

        for (const route of routes) {
            app.use(`/${route.replace(".ts", '')}`, require(`./routes/${route}`).default);
        }

        for (const table of tables) {
            const query = fs.readFileSync(`src/tables/${table}`).toString("utf-8");
            await client.execute(query);
        }

        app.use("", (_req, res) => {
            res.status(404).json({ message: "0_o the resource you were looking for was not found!" });
        });
    })();
} catch (err) {
    console.error(err);
}

export { client, redisClient };