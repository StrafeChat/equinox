import bodyParser from "body-parser";
import cors from "cors";
import express from "express";
import rateLimit from "express-rate-limit";
import fs from "fs";
import { FRONTEND, NEBULA, PORT, STARGATE, LIVEKIT } from './config';
import database from "./database";
import { Logger } from "./helpers/logger";
import path from "path";
import { redis } from "./database"

import { WebSocketServer } from "ws";
import { SignalingRelay, RoomManager } from "./portal";

const {
  LIVEKIT_API_KEY: key,
  LIVEKIT_API_SECRET: secret
} = process.env;

//-Initialize express-//
const app = express();

app.use(bodyParser.json({ limit: "25mb" }));

app.use(cors({
    origin: FRONTEND,
    methods: ['GET', 'POST', 'PUT', 'PATCH', 'DELETE'], 
    credentials: true,
}));

app.set('trust proxy', 1);
app.disable('x-powered-by');

const limiter = rateLimit({
    windowMs: 10 * 1000,
    max: 75,
  });

// CORS preflight handling
app.options('*', cors());

// Startup logic for equinox
const startServer = async () => {
    const port = PORT ?? 443;
    const versions = fs.readdirSync("src/routes");

    const mgr = new RoomManager({ key: key!, secret: secret! });
    app.use((req, _res, next) => {
      (req as any).portal = {
        manager: mgr
      } // TODO: add typings
      next();
    });

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

    const server = app.listen(port, () => {
        Logger.success(`Equinox is listening on ${port}!`);
    });
    const ws = new WebSocketServer({ server, path: "/portal/signaling/rtc" });

    ws.on("connection", (socket, req) => {
      const relay = new SignalingRelay(socket, req.url!, `ws://${LIVEKIT}`);
      const user = mgr.getUserByToken(relay.token!);
      console.log(user);
      if (!user) return;
      relay.on("close", () => {
        // TODO: register users leaving
        redis.publish("stargate", JSON.stringify({
          event: "voice_leave",
          data: {
            ...user,
            space_id: user.space
          }
        }));
      });

      // TODO: publish user to stargate
      redis.publish("stargate", JSON.stringify({
        event: "voice_join",
        data: {
          ...user,
          space_id: user.space
        }
      }));
    });
};

//  Start everything up
(async () => {
    Logger.start();
    await database.init();
    await startServer();
})();
