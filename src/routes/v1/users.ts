import { BatchDelete, BatchInsert, BatchUpdate } from "better-cassandra";
import { Router } from "express";
import { cassandra } from "../../database";
import UserByEmail from "../../database/models/UserByEmail";
import UserByUsernameAndDiscriminator from "../../database/models/UserByUsernameAndDiscriminator";
import { validateEditUserData, verifyToken } from "../../helpers/validator";
import { NEBULA } from "../../config";
import {
  ISpace,
  ISpaceMember,
  IUser,
  IUserByEmail,
  IUserByUsernameAndDiscriminator,
} from "../../types";
import SpaceMember from "../../database/models/SpaceMember";
import { generateToken } from "../../helpers/generator";
import Space from "../../database/models/Space";
import Room from "../../database/models/Room";
import User from "../../database/models/User";
const router = Router();

router.patch<
  string,
  {},
  {},
  {
    email: string;
    username: string;
    global_name: string;
    discriminator: number;
    locale: string;
    avatar: string;
  },
  {},
  { user: IUser }
>("/@me", verifyToken, validateEditUserData, async (req, res) => {
  if (req.body.email) {
    const users = await UserByEmail.select({
      $where: [{ equals: ["email", req.body.email] }],
    });
    if (users.length > 1)
      return res.status(400).json({ message: "Email is already in use" });
  }

  if (req.body.avatar) {
    const token = generateToken(
      res.locals.user.id,
      Number(res.locals!.user.created_at),
      res.locals.user!.secret
    );
    const response = await fetch(`${NEBULA}/avatars`, {
      method: "PATCH",
      headers: {
        "Content-Type": "application/json",
        Authorization: token,
      },
      body: JSON.stringify({
        data: req.body.avatar.replace(
          /^data:image\/(png|jpeg|jpg|gif|webp);base64,/,
          ""
        ),
      }),
    });

    if (!response.ok) {
      console.log(await response.json());
      return res.status(400).json({ message: "Something went wrong." });
    }

    const data = await response.json();

    res.locals.user!.avatar = data.hash;
  }

  const statements = [
    BatchUpdate<IUser>({
      name: "users",
      where: [{ equals: ["id", res.locals.user.id] }],
      set: {
        username: req.body.username || res.locals.user.username,
        email: req.body.email || res.locals.user.email,
        discriminator: req.body.discriminator || res.locals.user.discriminator,
        edited_at: Date.now(),
        locale: req.body.locale || res.locals.user.locale,
      },
    }),
  ];

  if (
    (req.body.username && req.body.username !== res.locals.user.username) ||
    (req.body.discriminator &&
      req.body.discriminator !== res.locals.user.discriminator)
  ) {
    const users = await UserByUsernameAndDiscriminator.select({
      $where: [
        { equals: ["username", req.body.username || res.locals.user.username] },
        {
          equals: [
            "discriminator",
            req.body.discriminator || res.locals.user.discriminator,
          ],
        },
      ],
      $prepare: true,
    });

    if (users.length > 0) {
      if (
        users[0].username === res.locals.user.username &&
        users[0].discriminator === res.locals.user.discriminator
      )
        return res
          .status(400)
          .json({ message: "Username and discriminator is already in use." });
      else if (users[0].username === res.locals.user.username)
        return res
          .status(400)
          .json({ message: "You're already using this username." });
      else
        return res
          .status(400)
          .json({ message: "You're already using this discriminator." });
    }

    statements.push(
      BatchDelete<IUserByUsernameAndDiscriminator>({
        name: "users_by_username_and_discriminator",
        where: [
          { equals: ["username", res.locals.user.username] },
          { equals: ["discriminator", res.locals.user.discriminator] },
        ],
      }),
      BatchInsert<IUserByUsernameAndDiscriminator>({
        name: "users_by_username_and_discriminator",
        data: {
          username: req.body.username || res.locals.user.username,
          discriminator:
            req.body.discriminator || res.locals.user.discriminator,
          id: res.locals.user.id || res.locals.user.discriminator,
        },
      })
    );
  }

  if (req.body.email && req.body.email !== res.locals.user.email)
    statements.push(
      BatchDelete<IUserByEmail>({
        name: "users_by_email",
        where: [{ equals: ["email", res.locals.user.email] }],
      }),
      BatchInsert<IUserByEmail>({
        name: "users_by_email",
        data: {
          email: req.body.email || res.locals.user.email,
          id: res.locals.user.id,
        },
      })
    );

  if (
    req.body.locale &&
    [
      "af-ZA",
      "sq-AL",
      "ar-SA",
      "az-AZ",
      "bg-BG",
      "cs-CZ",
      "da-DK",
      "de-DE",
      "el-GR",
      "en-GB",
      "en-US",
      "en-SF",
      "fr-FR",
      "nl-NL",
      "bn-BD",
    ].includes(req.body.locale)
  )
    res.locals.user.locale = req.body.locale;
  else if (req.body.locale)
    return res.status(400).json({ message: "Invalid locale code." });

  await cassandra.batch(statements, { prepare: true });

  res.locals.user.username = req.body.username || res.locals.user.username;
  res.locals.user.discriminator =
    req.body.discriminator || res.locals.user.discriminator;
  res.locals.user.email = req.body.email || res.locals.user.email;

  res.status(200).json({
    ...res.locals.user,
    password: undefined,
    secret: undefined,
    last_pass_reset: undefined,
  });
});

router.put("/@me/spaces/:space_id", verifyToken, async (req, res) => {
  if (res.locals.user.space_count >= 50)
    return res
      .status(403)
      .json({
        message: "You have reached the max amount of spaces you can join.",
      });
  if (isNaN(parseInt(req.params.space_id)))
    return res
      .status(400)
      .json({ message: "The space id you have provided is not correct." });

  const members = await SpaceMember.select({
    $where: [
      { equals: ["user_id", res.locals.user.id] },
      { equals: ["space_id", req.params.space_id] },
    ],
  });

  if (members.length > 0)
    return res.status(409).json({ message: "You are already in this space" });

  const spaces = await Space.select({
    $where: [{ equals: ["id", req.params.space_id] }],
  });

  if (spaces.length < 1)
    return res.status(404).json({ message: "The space was not found." });

  await cassandra.batch([
    BatchInsert<ISpaceMember>({
      name: "space_members",
      data: {
        user_id: res.locals.user.id,
        space_id: req.params.space_id,
        roles: [],
        joined_at: Date.now(),
        deaf: false,
        mute: false,
        avatar: null,
      },
    }),
    BatchUpdate<IUser>({
      name: "users",
      set: {
        space_ids: [...(res.locals.user.space_ids || []), req.params.space_id],
      },
      where: [{ equals: ["id", res.locals.user.id] }],
    }),
  ]);

  let space = spaces[0];

  const rooms = await Room.select({
    $where: [{ in: ["id", space.room_ids!] }],
  });

  return res.status(200).json({ space, rooms });
});

router.delete("/@me/spaces/:space_id", verifyToken, async (req, res) => {
  if (isNaN(parseInt(req.params.space_id)))
    return res
      .status(400)
      .json({ message: "The space id you have provided is not correct." });

  const members = await SpaceMember.select({
    $where: [
      { equals: ["user_id", res.locals.user.id] },
      { equals: ["space_id", req.params.space_id] },
    ],
  });

  if (members.length < 0)
    return res.status(400).json({ message: "You are not even in this space." });

  const spaces = await Space.select({
    $where: [{ equals: ["id", req.params.space_id] }],
  });

  if (spaces.length < 1)
    return res.status(404).json({ message: "The space was not found." });

  let space = spaces[0];

  if (space.owner_id == res.locals.user.id)
    return res
      .status(403)
      .json({
        message: "You cannot leave your own space, you must delete it instead.",
      });

  await cassandra.batch([
    BatchUpdate<IUser>({
      name: "users",
      set: {
        space_ids: (res.locals.user.space_ids || []).filter(
          (id) => id !== req.params.space_id
        ),
      },
      where: [{ equals: ["id", res.locals.user.id] }],
    }),
    BatchDelete<ISpaceMember>({
      name: "space_members",
      where: [
        { equals: ["user_id", res.locals.user.id] },
        { equals: ["space_id", req.params.space_id] },
      ],
    }),
  ]);

  const rooms = await Room.select({
    $where: [{ in: ["id", space.room_ids!] }],
  });

  return res.status(200).json({ space, rooms });
});

router.get("/:id", verifyToken, async (req, res) => {
  const user_id = req.params.id;

  try {
    const usersData = await User.select({
      $where: [{ equals: ["id", user_id] }],
      $limit: 1,
    });

    const user = usersData[0];

    if (!user)
      return res
        .status(404)
        .json({ message: "The user you were looking for does not exist." });

    res.status(200).json({
      id: user.id,
      username: user.username,
      display_name: user.global_name ?? user.username,
      avatar: user.avatar,
      banner: user.banner,
      about_me: user.about_me,
      accent_color: user.accent_color,
      discriminator: user.discriminator,
      global_name: user.global_name,
      flags: user.flags,
      public_plags: user.public_flags,
    });
  } catch (err) {
    console.log("ERROR: " + err);
    return res.status(500).json({ message: "Interal Server Error." });
  }
});

export default router;
