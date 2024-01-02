import crypto from "crypto";
import { Generator as SnowflakeGenerator } from 'snowflake-generator';

const snowflakes = new Map<number, SnowflakeGenerator>();
const chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';

export const generateSnowflake = (workerId: number) => {
    if (!snowflakes.has(workerId)) snowflakes.set(workerId, new SnowflakeGenerator(parseInt(process.env.SNOWFLAKE_EPOCH!), workerId));
    return snowflakes.get(workerId)!.generate().toString();
}

export const generateRandomString = (length = 12) => {
    let key = '';

    for (let i = 0; i < length; i++) {
        const randomIndex = crypto.randomBytes(length)[i] % chars.length;
        key += chars.charAt(randomIndex);
    }

    return key;
}

export const generateToken = (id: string, timestamp: number, secret: string) => {
    return `${Buffer.from(id).toString("base64url")}.${Buffer.from(timestamp.toString()).toString("base64url")}.${Buffer.from(secret).toString("base64url")}`;
}