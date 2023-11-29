import joi from "joi";

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


export class Validator {

    public static register(data: Register) {
        return joi.object({
            email: joi.string().email().required(),
            global_name: joi.string().alphanum().min(1).max(32).invalid("everyone", "here"),
            username: joi.string().alphanum().min(2).max(32).invalid("everyone", "here").required(),
            discriminator: joi.number().min(0).max(9999).required(),
            password: joi.string().pattern(new RegExp('^[a-zA-Z0-9]{3,30}$')).required(),
            confirm_password: joi.ref("password"),
            dob: joi.date().max(new Date(new Date().setFullYear(new Date().getFullYear() - 13))),
            locale: joi.string().regex(/^[a-z]{2}-[A-Z]{2}$/).required()
        }).validate(data);
    }
}