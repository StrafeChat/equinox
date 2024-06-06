require('dotenv').config();

const {
    SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER,
    SCYLLA_USERNAME,
    SCYLLA_PASSWORD,
    SCYLLA_KEYSPACE,
    PORT,
} = process.env as Record<string, string>;

if (!SCYLLA_CONTACT_POINTS) throw new Error('Missing an array of contact points for Cassandra or Scylla in the environmental variables.');
if (!SCYLLA_DATA_CENTER) throw new Error('Missing data center for Cassandra or Scylla in the environmental variables.');
if (!SCYLLA_KEYSPACE) throw new Error('Missing keyspace for Cassandra or Scylla in the environmental variables.');

export {
    PORT, SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER, SCYLLA_KEYSPACE, SCYLLA_PASSWORD, SCYLLA_USERNAME
};

export const PASSWORD_HASHING_SALT = process.env.PASSWORD_HASHING_SALT ? parseInt(process.env.PASSWORD_HASHING_SALT) : 12;

export const STARGATE = process.env.STARGATE ?? "wss://stargate.strafe.chat";
export const NEBULA = process.env.NEBULA ?? "https://nebula.strafe.chat";
export const FRONTEND = process.env.FRONTEND ?? "https://web.strafe.chat";
export const EQUINOX = process.env.EQUINOX ?? "https://equinox.strafe.chat";

export const USER_WORKER_ID = process.env.USER_WORKER_ID ? parseInt(process.env.USER_WORKER_ID) : 0;
export const SPACE_WORKER_ID = process.env.SPACE_WORKER_ID ? parseInt(process.env.SPACE_WORKER_ID) : 1;
export const ROOM_WORKER_ID = process.env.ROOM_WORKER_ID ? parseInt(process.env.ROOM_WORKER_ID) : 2;
export const MESSAGE_WORKER_ID = process.env.MESSAGE_WORKER_ID ? parseInt(process.env.MESSAGE_WORKER_ID) : 3;

export const STAFF_IDS = ["6411384778888578048"];

export enum RoomTypes {
    SECTION = 1,
    SPACE_TEXT = 2,
    SPACE_VOICE = 3
}

export const ErrorCodes = {
    INTERNAL_SERVER_ERROR: {
        CODE: 500,
        MESSAGE: "An internal server error has occured. Please try again later."
    },
    EMAIL_ALREADY_EXISTS: {
        CODE: 403,
        MESSAGE: "A user with this email already exists."
    },
    USERNAME_AND_DISCRIMINATOR_TAKEN: {
        CODE: 403,
        MESSAGE: "A user already has that username and discriminator."
    }
}