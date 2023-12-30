import { NextFunction, Request, Response } from "express";
import joi from "joi";
import { RegisterBody } from "../types";

export const JoiRegister = (req: Request<{}, {}, Partial<RegisterBody>>, res: Response, next: NextFunction) => {
    const schema = joi.object({
        email: joi.string().email().required().messages({
            "string.base": "The email should be a string.",
            "string.empty": "The email field is required.",
            "string.email": "The email that you have provided does not seem to be correct.",
            "string.required": "The email field is required.",
        }),
        global_name: joi.string().alphanum().min(1).max(32).invalid("everyone", "here").messages({
            "string.base": "The display name should be a string.",
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
        // confirm_password: joi.ref("password"),
        dob: joi.date().max(new Date(new Date().setFullYear(new Date().getFullYear() - 13))).required().messages({
            "date.base": "The date of birth must be a date.",
            "date.max": "You must be at least 13 years old to use strafe.",
            "date.required": "The date of birth field is required.",
        }),
        captcha: joi.string().required(),
        locale: joi.string().regex(/^[a-z]{2}-[A-Z]{2}$/).required().messages({
            "string.base": "The locale should be a string.",
            "string.empty": "The locale field is required.",
            "string.pattern.base": "The locale should be a string.",
        }),
    });

    const { error } = schema.validate(req.body);

    if (error) return res.status(400).json({ message: error.details[0].message });

    next();
}