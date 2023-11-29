import { Snowflake } from "./Snowflake";
import crypto from "crypto";

export class Generator {
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

    public static token = (id: string, last_pass_reset: number, secret: string) => {
        return Buffer.from(id).toString("base64url") + '.' + Buffer.from(last_pass_reset.toString()) + '.' + Buffer.from(secret).toString("base64url");
    }
}