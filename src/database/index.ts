import { Client } from 'better-cassandra';
import path from "path";
import { createClient } from 'redis';
import {
    SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER,
    SCYLLA_KEYSPACE,
    SCYLLA_PASSWORD,
    SCYLLA_USERNAME
} from '../config';

import { Logger } from "../helpers/logger";

const cassandra = new Client({
    contactPoints: JSON.parse(SCYLLA_CONTACT_POINTS),
    localDataCenter: SCYLLA_DATA_CENTER,
    keyspace: SCYLLA_KEYSPACE,
    credentials: (SCYLLA_USERNAME && SCYLLA_PASSWORD) ? {
        username: SCYLLA_USERNAME,
        password: SCYLLA_PASSWORD
    } : undefined,
    
    typesPath: path.join(__dirname, "/types"),
    modelsPath: path.join(__dirname, "/models"),

    logging: {
        success: Logger.success,
        info: Logger.info,
        error: Logger.error,
    }
});

const redis = createClient();

export const init = async () => {
    await cassandra.connect().catch(Logger.error);
    await redis.connect().catch(Logger.error);
}

export { cassandra, redis };
export default { init };
