import joi from "joi";
import { cassandra } from "..";
import { NextFunction, Request, Response } from "express";

export interface Register {
    email: string;
    global_name?: string;
    username: string;
    discriminator: number;
    password: string;
    confirm_password: string;
    dob: Date;
    locale: string;
}

export interface Login {
    email: string;
    password: string;
}

export class Validator {

    public static register(data: Register) {
        return joi.object({
            email: joi.string().email().required(),
            global_name: joi.string().alphanum().min(1).max(32).invalid("everyone", "here"),
            username: joi.string().alphanum().min(2).max(32).invalid("everyone", "here").required(),
            discriminator: joi.number().min(1).max(9999).required(),
            password: joi.string().min(8).max(128).required(),
            confirm_password: joi.ref("password"),
            dob: joi.date().max(new Date(new Date().setFullYear(new Date().getFullYear() - 13))),
            locale: joi.string().regex(/^[a-z]{2}-[A-Z]{2}$/).required()
        }).validate(data);
    }

    public static login(data: Login) {
        return joi.object({
            email: joi.string().email().required(),
            password: joi.string().min(8).max(128).required(),
        }).validate(data);
    }

    public static async verifyToken(req: Request, res: Response, next: NextFunction) {
        const token = req.headers["authorization"];
        if (!token) return res.status(403).json({ message: "Access Denied." });
        const parts = token.split(".");
        if (parts.length != 3) return res.status(403).json({ message: "Access Denied." });
        const id = atob(parts[0]);
        const timestamp = parseInt(atob(parts[1]));
        const secret = atob(parts[2]);
        const user = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.users
        WHERE id=?
        LIMIT 1;
        `, [id], { prepare: true });

        if (user.rowLength < 1) return res.status(403).json({ message: "Access Denied." });
        if (user.rows[0].get("last_pass_reset") > timestamp || user.rows[0].get("secret") != secret) return res.status(403).json({ message: "Access Denied." });
        req.body.user = user.rows[0];
        next();
    }

    public static async token(token?: string) {
        if (!token) return { code: 4004, message: "Access Denied." };
        const parts = token.split(".");
        if (parts.length != 3) return { code: 4004, message: "Access Denied." };
        const id = atob(parts[0]);
        const timestamp = parseInt(atob(parts[1]));
        const secret = atob(parts[2]);
        const user = await cassandra.execute(`
        SELECT * FROM ${cassandra.keyspace}.users
        WHERE id=?
        LIMIT 1;
        `, [id], { prepare: true });

        if (user.rowLength < 1) return { code: 4004, message: "Access Denied" };
        if (user.rows[0].get("last_pass_reset") > timestamp || user.rows[0].get("secret") != secret) return { code: 4004, message: "Access Denied." };
        return { user: user.rows[0] };
    }
}