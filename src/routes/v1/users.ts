import { BatchDelete, BatchInsert, BatchUpdate } from "better-cassandra";
import { Router } from "express";
import { cassandra } from "../../database";
import UserByEmail from "../../database/models/UserByEmail";
import UserByUsernameAndDiscriminator from "../../database/models/UserByUsernameAndDiscriminator";
import { JoiEditData, verifyToken } from "../../helpers/validator";
import { IUser, IUserByEmail, IUserByUsernameAndDiscriminator } from "../../types";
const router = Router();

router.patch<string, {}, {}, { email: string, username: string, discriminator: number }, {}, { user: IUser }>('/@me', verifyToken, JoiEditData, async (req, res) => {
    if (req.body.email) {
        const users = await UserByEmail.select({ $where: [{ equals: ["email", req.body.email] }] });
        if (users.length > 1) return res.status(400).json({ message: "Email is already in use" });
    }

    const statements = [
        BatchUpdate<IUser>({ name: "users", where: [{ equals: ["id", res.locals.user.id] }], set: { username: req.body.username || res.locals.user.username, email: req.body.email || res.locals.user.email, discriminator: req.body.discriminator || res.locals.user.discriminator, edited_at: Date.now() } }),
    ];

    if ((req.body.username && req.body.username !== res.locals.user.username) || (req.body.discriminator && req.body.discriminator !== res.locals.user.discriminator)) {
        const users = await UserByUsernameAndDiscriminator.select({ $where: [{ equals: ["username", req.body.username || res.locals.user.username] }, { equals: ["discriminator", req.body.discriminator || res.locals.user.discriminator] }], $prepare: true });

        if (users.length > 1) {
            if (users[0].username === res.locals.user.username && users[0].discriminator === res.locals.user.discriminator) return res.status(400).json({ message: "Username and discriminator is already in use" });
            else if (users[0].username === res.locals.user.username) return res.status(400).json({ message: "Username is already in use" });
            else return res.status(400).json({ message: "Discriminator is already in use" })
        }

        statements.push(
            BatchDelete<IUserByUsernameAndDiscriminator>({ name: "users_by_username_and_discriminator", where: [{ equals: ["username", res.locals.user.username] }, { equals: ["discriminator", res.locals.user.discriminator] }] }),
            BatchInsert<IUserByUsernameAndDiscriminator>({ name: "users_by_username_and_discriminator", data: { username: req.body.username || res.locals.user.username, discriminator: req.body.discriminator || res.locals.user.discriminator, id: res.locals.user.id || res.locals.user.discriminator, created_at: res.locals.user.created_at } })
        )
    }

    if (req.body.email && req.body.email !== res.locals.user.email) statements.push(
        BatchDelete<IUserByEmail>({ name: "users_by_email", where: [{ equals: ["email", res.locals.user.email] }] }),
        BatchInsert<IUserByEmail>({ name: "users_by_email", data: { email: req.body.email || res.locals.user.email, id: res.locals.user.id, created_at: res.locals.user.created_at } })
    )

    await cassandra.batch(statements, { prepare: true });

    res.locals.user.username = req.body.username || res.locals.user.username;
    res.locals.user.discriminator = req.body.discriminator || res.locals.user.discriminator;
    res.locals.user.email = req.body.email || res.locals.user.email;

    res.status(200).json({
        ...res.locals.user,
        password: undefined,
        secret: undefined,
        last_pass_reset: undefined
    });
});

export default router;