require('dotenv').config();

const {
    FRONTEND_URL,
    SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER,
    SCYLLA_USERNAME,
    SCYLLA_PASSWORD,
    SCYLLA_KEYSPACE,
    RESEND_API_KEY,
    PORT,
} = process.env as Record<string, string>;

if (!FRONTEND_URL) throw new Error('Missing FRONTEND_URL in environment variables.');
if (!SCYLLA_CONTACT_POINTS) throw new Error('Missing an array of contact points for Cassandra or Scylla in the environmental variables.');
if (!SCYLLA_DATA_CENTER) throw new Error('Missing data center for Cassandra or Scylla in the environmental variables.');
if (!SCYLLA_KEYSPACE) throw new Error('Missing keyspace for Cassandra or Scylla in the environmental variables.');
if (!RESEND_API_KEY) throw new Error('Missing RESEND_API_KEY in the environmental variables.');

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

export {
    FRONTEND_URL, PORT, RESEND_API_KEY, SCYLLA_CONTACT_POINTS,
    SCYLLA_DATA_CENTER, SCYLLA_KEYSPACE, SCYLLA_PASSWORD, SCYLLA_USERNAME
};
