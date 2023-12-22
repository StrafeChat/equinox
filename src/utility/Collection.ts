import { cassandra } from "..";
import Relationship from "../interfaces/Request";
import Room from "../interfaces/Room";
import User from "../interfaces/User";
import Generator from "./Generator";

type Selection<T> = ({ name: keyof T, value: T[keyof T] });

export default class Collection {

    public static users = {
        fetchById: async (id: string) => {
            return await Collection.fetchById<User>("users", id);
        },
        fetchByUsernameAndDiscrim: async (username: string, discriminator: number) => {
            const execute = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.users_by_username_and_discriminator
            WHERE username=? AND discriminator=?
            LIMIT 1;
            `, [username, discriminator], { prepare: true });

            if (execute.rowLength < 1) return null;

            return await Collection.fetchById<User>("users", execute.rows[0].get("id"));
        },
        fetchByEmail: async (email: string) => {
            const execute = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.users_by_email
            WHERE email=?
            LIMIT 1;
            `, [email], { prepare: true });

            if (execute.rowLength < 1) return null;

            return await Collection.fetchById<User>("users", execute.rows[0].get("id"));
        }
    };

    public static requests = {
        fetchSenderRequest: async (senderId: string, receiverId?: string) => {
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? ${receiverId && "AND receiver_id=?"}
            LIMIT 1;
            `, [senderId, receiverId], { prepare: true });

            return execution.rowLength ? execution.rows[0] as unknown as Relationship : null;
        },
        fetchManySenderRequests: async (senderId: string, receiverId?: string) => {
            const include = [senderId];
            if (receiverId) include.push(receiverId);
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_sender
            WHERE sender_id=? ${receiverId ? "AND receiver_id=?" : ''}
            `, include, { prepare: true });

            return execution.rowLength ? execution.rows as unknown[] as Relationship[] : [];
        },
        fetchReceiverRequest: async (receiverId: string, senderId?: string) => {
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=? ${senderId && "AND sender_id=?"}
            LIMIT 1;
            `, [receiverId, senderId], { prepare: true })

            return execution.rowLength ? execution.rows[0] as unknown as Relationship : null;
        },
        fetchManyReceiverRequests: async (receiverId: string, senderId?: string) => {
            const include = [receiverId];
            if (senderId) include.push(senderId);
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.requests_by_receiver
            WHERE receiver_id=? ${senderId ? "AND sender_id=?" : ''}
            `, include, { prepare: true });

            return execution.rowLength ? execution.rows as unknown[] as Relationship[] : [];
        },
    };

    public static rooms = {
        fetchById: async (id: string) => {
            return await this.fetchById<Room>("rooms", id);
        },
        fetchByUserId: async (userId: string) => {
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.room_recipients_by_user
            WHERE user_id=?
            `, [userId], { prepare: true });
        },
        fetchManyByUserId: async (userId: string) => {
            const rooms: Room[] = [];
            const execution = await cassandra.execute(`
            SELECT * FROM ${cassandra.keyspace}.room_recipients_by_user
            WHERE user_id=?;
            `, [userId], { prepare: true });

            if (execution.rowLength < 1) return rooms;

            for (const room of execution.rows) {
                const recipients = room.get("recipients") as string[];
                const recipientsTransformer: Partial<User>[] = [];

                for (const recipient of recipients) {
                    const user = await this.fetchById<User>("users", recipient);
                    if (user) recipientsTransformer.push(Generator.stripUserInfo(user));
                }

                rooms.push({ ...await this.fetchById<Room>("rooms", room.get("room_id")), recipients: [...recipientsTransformer] } as unknown as Room);
            }

            return rooms;
        }
    }

    private static async fetchById<T>(table: string, id: string) {
        const exeuction = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.${table}
        WHERE id=?
        LIMIT 1;
        `, [id], { prepare: true });

        return exeuction.rowLength < 1 ? null : exeuction.rows[0] as unknown as T;
    }

    public static async updateTable<T>(name: string, { update, where, prepare }: { update: Selection<T>[], where: Selection<T>[], prepare?: boolean }) {
        try {
            await cassandra.execute(`
            UPDATE ${cassandra.keyspace}.${name}
            SET ${update.map(($) => `${new String($.name)}=?`).join(", ")}
            WHERE ${where.map(($) => `${new String($.name)}=?`).join(" AND ")}
            IF EXISTS;
            `, [...update.map(($) => $.value), ...where.map(($) => $.value)], { prepare: prepare ?? true });
        } catch (error) {
            console.trace(`Error updating table ${name}: ${error}`);
        }
    }
}