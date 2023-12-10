import { cassandra } from "..";
import { Relationship } from "../interfaces/Request";
import { User } from "../interfaces/User";

export class Collection {

    public static users = {
        fetchById: async (id: string) => {
            return await Collection.fetchById("users", id);
        },
        fetchByUsernameAndDiscrim: async (username: string, discriminator: number) => {
            const execute = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.users_by_username_and_discriminator
            WHERE username=? AND discriminator=?
            LIMIT 1;
            `, [username, discriminator], { prepare: true });

            if (execute.rowLength < 1) return null;

            return await Collection.fetchById("users", execute.rows[0].get("id"));
        },
        fetchByEmail: async (email: string) => {
            const execute = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.users_by_email
            WHERE email=?
            LIMIT 1;
            `, [email], { prepare: true });

            if (execute.rowLength < 1) return null;

            return await Collection.fetchById("users", execute.rows[0].get("id"));
        }
    };

    public static requests = {
        fetchSenderRequest: async (senderId: string, receiverId?: string) => {
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? ${receiverId && "AND receiver_id=?"}
            LIMIT 1;
            `, [senderId, receiverId])

            return execution.rowLength ? execution.rows[0] as unknown as Relationship : null;
        },
        fetchManySenderRequests: async (senderId: string, receiverId?: string) => {
            const include = [senderId];
            if (receiverId) include.push(receiverId);
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? ${receiverId ? "AND receiver_id=?" : ''}
            `, include)

            return execution.rowLength ? execution.rows as unknown[] as Relationship[] : [];
        },
        fetchReceiverRequest: async (receiverId: string, senderId?: string) => {
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=? ${senderId && "AND sender_id=?"}
            LIMIT 1;
            `, [receiverId, senderId])

            return execution.rowLength ? execution.rows[0] as unknown as Relationship : null;
        },
        fetchManyReceiverRequests: async (receiverId: string, senderId?: string) => {
            const include = [receiverId];
            if (senderId) include.push(senderId);
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=? ${senderId ? "AND sender_id=?" : ''}
            `, include)

            return execution.rowLength ? execution.rows as unknown[] as Relationship[] : [];
        },
    };

    private static async fetchById(table: string, id: string) {
        const exeuction = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.${table}
        WHERE id=?
        LIMIT 1;
        `, [id], { prepare: true });

        return exeuction.rowLength < 1 ? null : exeuction.rows[0] as unknown as User;
    }
}