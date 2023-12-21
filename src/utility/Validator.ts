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
            email: joi.string().email().required().messages({
                "string.base": "The email should be a string.",
                "string.empty": "The email field is required.",
                "string.email": "The email that you have provided does not seem to be correct.",
                "string.required": "The email field is required.",
            }),
            global_name: joi.string().alphanum().min(1).max(32).invalid("everyone", "here").messages({
                "string.base": "The display name should be a string.",
                "string.empty": "The display name cannot be empty.",
                "string.alphanum": "The display name should only include numbers and letters.",
                "string.min": "The display name cannot be less than 1 character long.",
                "string.max": "The display name cannot be more than 32 characters long.",
                "any.invalid": "Your display name cannot be 'everyone' or 'here'.",
            }),
            username: joi.string().alphanum().min(2).max(32).invalid("everyone", "here").required().messages({
                "string.base": "The username should be a string.",
                "string.empty": "The username cannot be empty.",
                "string.alphanum": "The username should only include numbers and letters.",
                "string.min": "The username cannot be less than 2 character long.",
                "string.max": "The username cannot be more than 32 characters long.",
                "any.invalid": "Your username cannot be 'everyone' or 'here'.",
                "string.required": "The username field is required.",
            }),
            discriminator: joi.number().integer().min(1).max(9999).required().messages({
                "number.base": "The discriminator should be an integer.",
                "number.infinity": "The discriminator cannot be infinite",
                "number.integer": "The discriminator should be an integer.",
                "number.min": "The discriminator must be more than 0.",
                "number.max": "The discriminator must be less than 9999.",
                "number.required": "The discriminator field is required.",
            }),
            password: joi.string().min(8).max(128).required().messages({
                "string.base": "The password should be a string.",
                "string.empty": "The password cannot be empty.",
                "string.min": "The password cannot be less than 8 characters long.",
                "string.max": "The password cannot be more than 128 characters long.",
                "string.required": "The password field is required.",
            }),
            confirm_password: joi.ref("password"),
            dob: joi.date().max(new Date(new Date().setFullYear(new Date().getFullYear() - 13))).required().messages({
                "date.base": "The date of birth must be a date.",
                "date.max": "You must be at least 13 years old to use strafe.",
                "date.required": "The date of birth field is required.",
            }),
            locale: joi.string().regex(/^[a-z]{2}-[A-Z]{2}$/).required().messages({
                "string.base": "The locale should be a string.",
                "string.empty": "The locale field is required.",
                "string.pattern.base": "The locale should be a string.",
            })
        }).validate(data);
    }

    public static login(data: Login) {
        return joi.object({
            email: joi.string().email().required().messages({
                "string.base": "The email should be a string.",
                "string.empty": "The email field is required.",
                "string.email": "The email that you have provided does not seem to be correct.",
                "string.required": "The email field is required."
            }),
            password: joi.string().min(8).max(128).required().messages({
                "string.base": "The password should be a string.",
                "string.empty": "The password field is required.",
                "string.min": "The password is incorrect.",
                "string.max": "The password is incorrect.",
                "string.required": "The password field is required."
            }),
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
        (req as any).user = user.rows[0];
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