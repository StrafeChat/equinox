import { Snowflake } from "../util/Snowflake"
import crypto from "crypto";

const snowflakes = new Map<number, Snowflake>();
const chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';

export const generateSnowflake = (workerId: number) => {
    if (!snowflakes.has(workerId)) snowflakes.set(workerId, new Snowflake(parseInt(process.env.SNOWFLAKE_EPOCH!, 10), workerId));
    return snowflakes.get(workerId)!.generate();
}

export const generateRandomString = (length = 12) => {
    const randomBytes = crypto.randomBytes(length);
    let key = '';

    for (let i = 0; i < length; i++) {
        const randomIndex = randomBytes[i] % chars.length;
        key += chars.charAt(randomIndex);
    }

    return key;
}

export const generateToken = (id: string, last_pass_reset: number, secret: string) => {

}