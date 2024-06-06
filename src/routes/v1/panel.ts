import { Router } from "express";
import bcrypt from "bcrypt";
import { IUser, LoginBody } from "../../types";
import UserByEmail from "../../database/models/UserByEmail";
import User from "../../database/models/User";
import { generateToken } from "../../helpers/generator";
import { ErrorCodes, STAFF_IDS } from "../../config";
import { verifyToken } from "../../helpers/validator";

const router = Router();

router.get("/@me", verifyToken, (req, res) => {
  if (!STAFF_IDS.includes(res.locals.user.id))
    return res.status(401).json({ message: "Not Authorized." });
  res.status(200).json({ ...res.locals.user });
});

router.get("/users/:userId", verifyToken, async (req, res) => {
  if (!STAFF_IDS.includes(res.locals.user.id))
    return res.status(401).json({ message: "Not Authorized." });
  if (isNaN(parseInt(req.params.userId)))
    return res
      .status(400)
      .json({ message: "The id you have provided is not correct!" });

  const users = await User.select({
    $include: [
      "avatar",
      "email",
      "username",
      "global_name",
      "discriminator",
      "flags",
      "id",
      "dob",
      "created_at",
      "about_me",
      "banned",
      "presence",
      "locale",
    ],
    $where: [{ equals: ["id", req.params.userId] }],
  });

  if (users.length < 1)
    return res
      .status(404)
      .json({ message: `Could not find a user by that id.` });

  return res.status(200).json({
    ...users[0],
    display_name: users[0].global_name ?? users[0].username,
  });
});

router.patch("/users/:userId", verifyToken, async (req, res) => {
  try {
    if (!STAFF_IDS.includes(res.locals.user!.id))
      return res.status(401).json({ message: "Not Authorized" });

    const { email, username, global_name, discriminator, flags } = req.body;

    const updateFields: { [key: string]: any } = {};
    if (email) updateFields.email = email;
    if (username) updateFields.username = username;
    if (global_name) updateFields.global_name = global_name;
    if (discriminator) updateFields.discriminator = discriminator >>> 0;
    if (flags) updateFields.flags = flags >>> 0;

    if (Object.keys(updateFields).length === 0) {
      return res
        .status(400)
        .json({ message: "Please provide data to update!" });
    }

    const users = await User.select({
      $include: ["avatar", "email", "username", "global_name", "discriminator", "flags", "id"],
      $where: [{ equals: ["id", req.params.userId] }],
    });
    if (users.length < 1)
      return res
        .status(404)
        .json({ message: "The user you provided was not found." });

    User.update({
      $where: [
        {
          equals: ["id", users[0].id],
        },
      ],
      $set: {
        email: email,
        username: username,
        global_name: global_name,
        discriminator: discriminator,
        flags: flags,
      },
      $prepare: true,
    });

    res.status(200).json({ message: "Success!" });
  } catch (err) {
    console.log(err);
    return res.status(500).json({ message: "Internal Server Error" });
  }
});

export default router;
