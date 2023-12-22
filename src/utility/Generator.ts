import { Snowflake } from "./Snowflake";
import { types } from "cassandra-driver";
import crypto from "crypto";
import User from "../interfaces/User";

export default class Generator {
    public static snowflake = new Snowflake();

    public static randomKey = (length = 12) => {
        const characters = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
        const randomBytes = crypto.randomBytes(length);
        let key = '';

        for (let i = 0; i < length; i++) {
            const randomIndex = randomBytes[i] % characters.length;
            key += characters.charAt(randomIndex);
        }

        return key;
    };

    public static token = (id: string, timestamp: number, secret: string) => {
        return Buffer.from(id).toString("base64url") + '.' + Buffer.from(timestamp.toString()).toString("base64url") + '.' + Buffer.from(secret).toString("base64url");
    }

    public static rowToObj = (row: types.Row) => {
        const result: Record<string, any> = {};

        row.keys().forEach(columnName => {
            result[columnName] = row.get(columnName);
        });

        return result;
    }

    public static stripSpecific<T>(data: any, keys: (keyof T)[]): Partial<T> {
        const strippedData: Partial<T> = { ...data };

        for (const key of keys) {
            if (strippedData[key]) strippedData[key] = undefined;
        }

        return strippedData;
    }

    public static stripUserInfo(info: User) {
        return {
            ...info,
            dob: undefined,
            hide: undefined,
            last_pass_reset: undefined,
            password: undefined,
            secret: undefined,
            email: undefined,
        }
    }
}