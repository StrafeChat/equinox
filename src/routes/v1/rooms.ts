import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import rateLimit from "express-rate-limit";
import { Request } from "express";
import Room from "../../database/models/Room";
import { cassandra, redis } from "../../database";
import multer from "multer";
import {
  generateInviteCode,
  generateSnowflake,
  generateToken,
} from "../../helpers/generator";
import { MESSAGE_WORKER_ID, ROOM_WORKER_ID, NEBULA } from "../../config";
import {
  IInvite,
  IMessage,
  IMessageByRoom,
  IRoom,
  IRoomUnreads,
  MessageAttachment,
  MessageSudo,
} from "../../types";
import Message from "../../database/models/Message";
import Space from "../../database/models/Space";
import SpaceMember from "../../database/models/SpaceMember";
import Invite from "../../database/models/Invite";
import { BatchDelete, BatchInsert } from "better-cassandra";
import MessageByRoom from "../../database/models/MessageByRoom";
import User from "../../database/models/User";
import RoomUnread from "../../database/models/RoomUnread";
import RoomMention from "../../database/models/RoomMention";

const router = Router();
const upload = multer();

const messageCreateLimit = rateLimit({
  windowMs: 1 * 100,
  max: 50,
  keyGenerator: (req: Request) => {
    return req.headers["authorization"]!;
  },
});

const messageEditLimit = rateLimit({
  windowMs: 1 * 100,
  max: 75,
  keyGenerator: (req: Request) => {
    return req.headers["authorization"]!;
  },
});

const roomCreateLimit = rateLimit({
  windowMs: 1 * 100,
  max: 25,
  keyGenerator: (req: Request) => {
    return req.headers["authorization"]!;
  },
});

router.post("/", verifyToken, roomCreateLimit, async (req, res) => {
  const { space_id, name, type, parent_id } = req.body;

  try {
    if (!name || typeof type !== "number" || type < 0)
      return res.status(400).json({
        message:
          "You need to provide a vaild name and type for the room. If you want it to be in a space then you must provide a vaild space_id.",
      });

    if (space_id) {
      const spaces = await Space.select({
        $where: [{ equals: ["id", space_id] }],
        $limit: 1,
      });

      const space = spaces[0];

      const id = generateSnowflake(ROOM_WORKER_ID);
      const roomIds = new Set(space.room_ids || []);
      roomIds.add(id);

      if (!space)
        return res
          .status(404)
          .json({ message: "The space you were looking for does not exist." });

      if (res.locals.user.id !== space.owner_id)
        return res.status(403).json({
          message: "You do not have permission to create rooms on this space.",
        });

      switch (type) {
        case 0:
          const Section: IRoom = {
            id,
            type,
            space_id,
            position: 0,
            name,
            created_at: Date.now(),
            edited_at: Date.now(),
            owner_id: null,
            permission_overwrites: [],
            topic: null,
            last_message_id: null,
            bitrate: null,
            user_limit: null,
            rate_limit: null,
            recipients: [],
            icon: null,
            parent_id: null,
            last_pin_timestamp: null,
            rtc_region: null,
          };

          await Room.insert(Section, { prepare: true });
          await Space.update({
            $where: [
              { equals: ["id", space.id] },
              { equals: ["created_at", space.created_at] },
            ],
            $set: { room_ids: Array.from(roomIds) },
          });

          res.status(200).json(Section);
          break;
        case 1:
          const TextRoom: IRoom = {
            id,
            type,
            space_id,
            position: 0,
            name,
            created_at: Date.now(),
            edited_at: Date.now(),
            owner_id: null,
            permission_overwrites: [],
            topic: null,
            last_message_id: null,
            bitrate: null,
            user_limit: null,
            rate_limit: null,
            recipients: [],
            icon: null,
            parent_id: parent_id || null,
            last_pin_timestamp: null,
            rtc_region: null,
          };

          await Room.insert(TextRoom, { prepare: true });
          await Space.update({
            $where: [
              { equals: ["id", space.id] },
              { equals: ["created_at", space.created_at] },
            ],
            $set: { room_ids: Array.from(roomIds) },
          });

          res.status(200).json(TextRoom);
          break;
        case 2:
          const VoiceRoom: IRoom = {
            id,
            type,
            space_id,
            position: 0,
            name,
            created_at: Date.now(),
            edited_at: Date.now(),
            owner_id: null,
            permission_overwrites: [],
            topic: null,
            last_message_id: null,
            bitrate: null,
            user_limit: null,
            rate_limit: null,
            recipients: [],
            icon: null,
            parent_id: parent_id || null,
            last_pin_timestamp: null,
            rtc_region: null,
          };

          await Room.insert(VoiceRoom, { prepare: true });
          await Space.update({
            $where: [
              { equals: ["id", space.id] },
              { equals: ["created_at", space.created_at] },
            ],
            $set: { room_ids: Array.from(roomIds) },
          });

          res.status(200).json(VoiceRoom);
          break;
        default:
          return res
            .status(400)
            .json({ message: "This room does not support message sending." });
      }
    }
  } catch (err) {
    console.error(err);
    return res.status(500).json({ message: "Internal Server Error." });
  }
});

router.post(
  "/:room_id/messages",
  verifyToken,
  messageCreateLimit,
  upload.array("attachments"),
  async (req, res) => {
    const { room_id } = req.params;

    if (isNaN(parseInt(room_id)))
      return res.status(400).json({ message: "Invalid room ID." });

    const { content, message_reference_id, embeds, sudo, attachments } =
      req.body;

    if (!content && !embeds && !attachments)
      return res
        .status(400)
        .json({ message: "Message content or attachment is required." });

    if (content && typeof content !== "string")
      return res
        .status(400)
        .json({ message: "Message content must be a string." });

    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });
    const room = rooms[0];

    if (!room)
      return res.status(404).json({ message: "The room does not exist." });

    const message_id = generateSnowflake(MESSAGE_WORKER_ID);

    let userMentions: string[] = [];

    const userMentionRegex = /<@!?(\d{17,19})>/g;
    
    const matches = content.matchAll(userMentionRegex);
    
    for (const match of matches) {
      const userId = match[1];

      const users = await User.select({
        $where: [{equals: ["id", userId]}],
        $limit: 1
      })
      if (users[0]) userMentions.push(userId);
    } 

    try {
      let attachmentList: any = [];

      if (attachments) {
        for (const attachment of attachments) {
          const { file, name, type } = attachment;

          const token = generateToken(
            res.locals.user.id,
            Number(res.locals!.user.created_at),
            res.locals.user!.secret
          );

          const response = await fetch(`${NEBULA}/attachments`, {
            method: "POST",
            headers: {
              Authorization: token,
              "Content-Type": "application/json",
            },
            body: JSON.stringify({ file, name, type }),
          });

          if (!response.ok) {
            console.error(await response.json());
            continue;
          }

          const { url, metadata } = await response.json();

          attachmentList.push({
            name,
            url,
            type,
            width: metadata.width,
            height: metadata.height,
          });
        }
      }

      const messageData = {
        room_id,
        content: content || null,
        id: message_id,
        author_id: res.locals.user.id,
        space_id: room.space_id!,
        system: res.locals.user.system,
        tts: false,
        attachments: attachmentList,
        embeds: embeds || [],
        flags: 0,
        mention_everyone: false,
        mention_roles: [],
        mention_rooms: [],
        mentions: userMentions,
        message_reference_id: message_reference_id ?? null,
        pinned: false,
        sudo: sudo || null,
        reactions: [],
        stickers: [],
        thread_id: null,
        webhook_id: null,
        edited_at: null,
        created_at: Date.now(),
      };

      await cassandra.batch(
        [
          BatchInsert<IMessage>({ name: "messages", data: messageData }),
          BatchInsert<IMessageByRoom>({
            name: "messages_by_room",
            data: { id: message_id, room_id },
          }),
        ],
        { prepare: true }
      );

      await redis.publish(
        "stargate",
        JSON.stringify({
          event: "message_create",
          data: {
            room_type: room.type,
            space_id: room.space_id,
            room_id: room.id,
            content: content || null,
            attachments: attachmentList,
            embeds: embeds || [],
            sudo: sudo || null,
            message_reference_id: message_reference_id || null,
            id: message_id,
            created_at: messageData.created_at,
            mentions: userMentions,
            nonce: 1,
            member: {
              user: {
                id: res.locals.user.id,
                username: res.locals.user.username,
                discriminator: res.locals.user.discriminator,
                global_name: res.locals.user.global_name,
                flags: res.locals.user.flags,
                display_name:
                  res.locals.user.global_name || res.locals.user.username,
                avatar: res.locals.user.avatar,
                bot: res.locals.user.bot,
                presence: res.locals.user.presence,
                created_at: res.locals.user.created_at,
              },
            },
            author: {
              id: res.locals.user.id,
              username: res.locals.user.username,
              discriminator: res.locals.user.discriminator,
              global_name: res.locals.user.global_name,
              flags: res.locals.user.flags,
              display_name:
                res.locals.user.global_name || res.locals.user.username,
              avatar: res.locals.user.avatar,
              bot: res.locals.user.bot,
              presence: res.locals.user.presence,
              created_at: res.locals.user.created_at,
            },
          },
        })
      );

      if (messageData.space_id) {
        const members = await SpaceMember.select({
          $where: [{ equals: ["space_id", messageData.space_id] }],
        });

        members.forEach(async (member) => {
          if (member.user_id == res.locals.user.id) return;

          let mentioned = false;
          if (messageData.mentions.includes(member.user_id!)) mentioned = true;
          
        if (mentioned) {
          const roomMentions = await RoomMention.select({
            $where: [{equals: ["room_id", messageData.room_id]}, {equals: ["user_id", member.user_id]}]
          })

          if (roomMentions[0]) {
             RoomMention.update({
              $where: [{equals: ["room_id", messageData.room_id]}, {equals: ["user_id", member.user_id]}],
              $set: {message_ids: [...roomMentions[0].message_ids!, messageData.id]},
              $prepare: true
             })
          } else {
            RoomMention.insert({
              user_id: member.user_id,
              room_id: messageData.room_id,
              message_ids: [messageData.id]
            }, {
              prepare: true
            })
          }
        }

          messageData.mentions
          RoomUnread.insert(
            {
              room_id: messageData.room_id,
              message_id: messageData.id,
              user_id: member.user_id,
            },
            { prepare: true }
          );
        });
      }

      // Return the created message data
      res.status(200).json({ ...messageData, nonce: 0 });
    } catch (error) {
      console.error("Error creating message:", error);
      res.status(500).json({ message: "Failed to create message." });
    }
  }
);

router.patch(
  "/:room_id/messages/:message_id",
  verifyToken,
  messageEditLimit,
  async (req, res) => {
    const { room_id, message_id } = req.params;

    try {
      if (isNaN(parseInt(room_id)))
        return res.status(400).json({ message: "Invalid room ID." });
      if (isNaN(parseInt(message_id)))
        return res.status(400).json({ message: "Invalid message ID." });

      const { content, embeds } = req.body;

      if (!content && !embeds)
        return res
          .status(400)
          .json({ message: "You must provide content for your message." });

      if (content && typeof content !== "string")
        return res
          .status(400)
          .json({ message: "Message content must be a string." });
      if (content && content.length > 2024)
        return res.status(400).json({
          message: "Message content must be less than 2,024 characters long.",
        });

      const rooms = await Room.select({
        $where: [{ equals: ["id", room_id] }],
        $limit: 1,
      });

      const room = rooms[0];

      if (!room)
        return res
          .status(404)
          .json({ message: "The room you were looking for does not exist." });

      const messages = await Message.select({
        $where: [{ equals: ["id", message_id] }],
        $limit: 1,
      });

      const message = messages[0];

      if (!message)
        return res.status(404).json({
          message: "The message you were looking for does not exist.",
        });

      Message.update({
        $where: [
          { equals: ["id", message.id] },
          { equals: ["created_at", message.created_at] },
        ],
        $prepare: true,
        $set: {
          content: content ?? null,
          embeds: embeds ?? [],
          edited_at: Date.now(),
        },
      });

      await redis.publish(
        "stargate",
        JSON.stringify({
          event: "message_update",
          data: {
            id: message.id,
            room_id: room.id,
            space_id: room.space_id,
            content: content ?? null,
            embeds: embeds ?? [],
            created_at: Number(message.created_at!),
            edited_at: Date.now(),
            author: {
              id: res.locals.user.id,
              username: res.locals.user.username,
              display_name:
                res.locals.user.global_name ?? res.locals.user.username,
              discriminator: res.locals.user.discriminator,
              global_name: res.locals.user.global_name,
              avatar: res.locals.user.avatar,
              bot: res.locals.user.bot,
              presence: res.locals.user.presence,
              created_at: res.locals.user.created_at,
            },
          },
        })
      );

      return res.status(200).json({
        message: {
          ...message,
          content,
          author: {
            id: res.locals.user.id,
            username: res.locals.user.username,
            display_name:
              res.locals.user.global_name ?? res.locals.user.username,
            discriminator: res.locals.user.discriminator,
            global_name: res.locals.user.global_name,
            avatar: res.locals.user.avatar,
            bot: res.locals.user.bot,
            presence: res.locals.user.presence,
          },
        },
      });
    } catch (err) {
      console.error(err);
      res.status(500).json({ message: "Interal Server Error" });
    }
  }
);

router.delete(
  "/:room_id/messages/:message_id",
  verifyToken,
  async (req, res) => {
    const { room_id, message_id } = req.params;

    try {
      if (isNaN(parseInt(room_id)))
        return res.status(400).json({ message: "Invalid room ID." });
      if (isNaN(parseInt(message_id)))
        return res.status(400).json({ message: "Invalid message ID." });

      const rooms = await Room.select({
        $where: [{ equals: ["id", room_id] }],
        $limit: 1,
      });

      const room = rooms[0];

      if (!room)
        return res
          .status(404)
          .json({ message: "The room you were looking for does not exist." });

      const messages = await Message.select({
        $where: [{ equals: ["id", message_id] }],
        $limit: 1,
      });

      const message = messages[0];

      if (!message)
        return res.status(404).json({
          message: "The message you were looking for does not exist.",
        });

      await cassandra.batch(
        [
          BatchDelete<IMessage>({
            name: "messages",
            where: [
              { equals: ["id", message.id] },
              { equals: ["created_at", message.created_at] },
            ],
          }),
          BatchDelete<IMessageByRoom>({
            name: "messages_by_room",
            where: [
              { equals: ["room_id", message.room_id] },
              { equals: ["id", message.id] },
            ],
          }),
        ],
        { prepare: true }
      );

      await redis.publish(
        "stargate",
        JSON.stringify({
          event: "message_delete",
          data: {
            id: message.id,
            room_id: room.id,
            space_id: room.space_id,
            content: message.content,
            author: {
              id: res.locals.user.id,
              username: res.locals.user.username,
              display_name:
                res.locals.user.global_name ?? res.locals.user.username,
              discriminator: res.locals.user.discriminator,
              global_name: res.locals.user.global_name,
              avatar: res.locals.user.avatar,
              bot: res.locals.user.bot,
            },
          },
        })
      );

      return res.status(202).send({
        status: "Sent",
      });
    } catch (err) {
      console.error(err);
      res.status(500).json({ message: "Interal Server Error" });
    }
  }
);

router.post("/:room_id/ack", verifyToken, async (req, res) => {
  const { room_id } = req.params;
  const user = res.locals.user;

  const unreads = await RoomUnread.select({
    $where: [
      { equals: ["room_id", room_id] },
      { equals: ["user_id", user.id] },
    ], // this can't be right, what's the primary key here?
    $limit: 1, // how about a table by user id, and we select the correct room manually
  });

  const unread = unreads[0];

  if (!unread) {
    return res.status(400).json({
      message: "You have no unreads for this room.",
    });
  }

  await RoomUnread.delete({
    $where: [
      { equals: ["room_id", room_id] },
      { equals: ["user_id", user.id] },
    ],
  });

  return res.sendStatus(200);
});

router.post("/:room_id/typing", verifyToken, async (req, res) => {
  const { room_id } = req.params;

  try {
    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });

    const room = rooms[0];

    if (!room)
      return res
        .status(404)
        .json({ message: "The room you were looking for does not exist." });

    await redis.publish(
      "stargate",
      JSON.stringify({
        event: "typing_start",
        data: {
          room_id: room.id,
          space_id: room.space_id,
          user: {
            id: res.locals.user.id,
            username: res.locals.user.username,
            display_name:
              res.locals.user.global_name ?? res.locals.user.username,
            discriminator: res.locals.user.discriminator,
            global_name: res.locals.user.global_name,
            avatar: res.locals.user.avatar,
            bot: res.locals.user.bot,
          },
        },
      })
    );

    return res.status(204).json({ message: "OK" });
  } catch (err) {
    console.error(err);
    res.status(500).json({ message: "Interal Server Error" });
  }
});

router.post("/:room_id/invites", verifyToken, async (req, res) => {
  const { room_id } = req.params;

  try {
    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });

    const room = rooms[0];

    if (!room)
      return res
        .status(404)
        .json({ message: "The room you were looking for does not exist." });

    if (!room.space_id)
      return res.status(400).json({
        message:
          "You can only create invites for space rooms at the moment. You cannot generate invites for PMs or group PMs.",
      });

    const members = await SpaceMember.select({
      $where: [
        { equals: ["user_id", res.locals.user.id] },
        { equals: ["space_id", room.space_id] },
      ],
      $limit: 1,
    });

    const member = members[0];

    if (!member)
      return res.status(403).json({
        message: "You must be in this space to create an invite for it.",
      });

    // TODO: if(member.permissions == [insert create invite permission here] || or the space owner) //

    const code = generateInviteCode();

    const invite: IInvite = {
      code,
      inviter_id: res.locals.user.id,
      space_id: room.space_id,
      room_id: room_id,
      uses: 0,
      max_uses: null,
      vanity: false,
      expires_at: null,
      created_at: Date.now(),
    };

    await Invite.insert(invite, { prepare: true });

    // await redis.publish(
    //   "stargate",
    //   JSON.stringify({
    //     event: "invite_create",
    //     data: {
    //       room_id: room.id,
    //       space_id: room.space_id,
    //       inviter: {
    //         id: res.locals.user.id,
    //         username: res.locals.user.username,
    //         display_name: res.locals.user.global_name ?? res.locals.user.username,
    //         discriminator: res.locals.user.discriminator,
    //         global_name: res.locals.user.global_name,
    //         avatar: res.locals.user.avatar,
    //         bot: res.locals.user.bot,
    //       }
    //     },
    //   })
    // );

    return res.status(200).json({ invite });
  } catch (err) {
    console.log("Error: " + err);
    res.status(500).json({ message: "Internal Server Error." });
  }
});

router.get("/:room_id/invites/:invite_code", verifyToken, async (req, res) => {
  const { room_id, invite_code } = req.params;

  try {
    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });

    const room = rooms[0];

    if (!room)
      return res
        .status(404)
        .json({ message: "The room you were looking for does not exist." });

    const invites = await Invite.select({
      $where: [{ equals: ["code", invite_code] }],
      $limit: 1,
    });

    const invite = invites[0];

    if (!invite)
      return res
        .status(404)
        .json({ message: "The invite you were looking for does not exist." });

    const spaces = await Space.select({
      $where: [{ equals: ["id", invite.space_id] }],
      $limit: 1,
    });

    const space = spaces[0];

    const member_count = await SpaceMember.select({
      $where: [{ equals: ["space_id", space.id] }],
    });

    return res.status(200).json({
      invite: { ...invite, created_at: Number(invite.created_at) },
      space: {
        id: invite.space_id,
        name: space.name,
        icon: space.icon,
        name_acronym: space.name_acronym,
        banner: space.banner,
        description: space.description,
        member_count: member_count.length,
      },
    });
  } catch (err) {
    console.log("ERROR: " + err);
    return res.status(500).json({ message: "Interal Server Error." });
  }
});

router.delete("/:room_id", verifyToken, async (req, res) => {
  try {
    const { room_id } = req.params;

    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });

    const room = rooms[0];

    if (!room)
      return res
        .status(404)
        .json({ message: "The room you were looking for does not exist." });

    const spaces = await Space.select({
      $where: [{ equals: ["id", room.space_id!] }],
      $limit: 1,
    });

    const space = spaces[0];

    if (!space)
      return res
        .status(404)
        .json({ message: "The space you were looking for does not exist." });

    if (res.locals.user.id !== space.owner_id)
      return res.status(403).json({
        message:
          "You do not have permission to delete this room on this space.",
      });

    const roomIds = new Set(space.room_ids);
    roomIds.delete(room.id!);

    await Room.delete({
      $where: [
        { equals: ["id", room.id] },
        { equals: ["created_at", room.created_at] },
      ],
    });
    await Space.update({
      $where: [
        { equals: ["id", space.id] },
        { equals: ["created_at", space.created_at] },
      ],
      $set: { room_ids: Array.from(roomIds) },
    });

    await redis.publish(
      "stargate",
      JSON.stringify({
        event: "room_delete",
        data: {
          room_id: room.id,
          space_id: room.space_id,
          space: {
            id: space.id,
            name: space.name,
          },
        },
      })
    );

    return res.status(200).json({ room });
  } catch (err) {
    console.error();
    return res.status(500).json({ message: "Internal Server Error." });
  }
});

router.get("/:room_id/messages", verifyToken, async (req, res) => {
  try {
    const { room_id } = req.params;
    const { around, before, after, limit } = req.query;

    const rooms = await Room.select({
      $where: [{ equals: ["id", room_id] }],
      $limit: 1,
    });

    const room = rooms[0];

    if (!room)
      return res
        .status(404)
        .json({ message: "The room you were looking for does not exist." });

    let member = await SpaceMember.select({
      $limit: 1,
      $where: [
        { equals: ["space_id", room.space_id!] },
        { equals: ["user_id", res.locals.user.id] },
      ],
    });

    if (!member[0])
      return res
        .status(401)
        .json({ message: "You do not have permission to view this content." });

    let operators: any[] = [{ equals: ["room_id", room.id] }];

    if (before) operators.push({ lessThan: ["id", before] });
    if (after) operators.push({ greaterThan: ["id", after] });

    let messageIds = await MessageByRoom.select({
      $limit: limit ? parseInt(limit as string, 10) : 100,
      $where: operators,
    });

    let messagesArray: any = [];

    await Promise.all(
      messageIds.map(async (messageByRoom) => {
        let messagesResult = (
          await Message.select({
            $limit: 1,
            $where: [{ equals: ["id", messageByRoom.id] }],
          })
        ).sort((a, b) => parseInt(b.id!) - parseInt(a.id!));

        const message: any = messagesResult[0];

        let authorResult = await User.select({
          $limit: 1,
          $where: [{ equals: ["id", message.author_id] }],
        });

        let memberResult: any = await SpaceMember.select({
          $limit: 1,
          $where: [
            { equals: ["space_id", room.space_id!] },
            { equals: ["user_id", message.author_id!] },
          ],
        });

        if (message.embeds && message.embeds[0]) {
          message.embeds.forEach((embed: any) => {
            if (embed.timestamp) embed.timestamp = embed.timestamp.getTime();
          });
        }

        let member = memberResult[0];

        message.author = authorResult[0];
        message.created_at = message.created_at.getTime();
        message.author.username = authorResult[0].username ?? "Deleted User";
        message.author.discriminator = authorResult[0].discriminator ?? 0;
        message.author.display_name =
          authorResult[0].global_name ?? message.author.username;

        member.user = message.author;
        message.member = member;

        messagesArray.push(message);
      })
    );

    const messages = messagesArray;

    res.status(200).json({ messages });
  } catch (err) {
    console.error(err);
    return res.status(500).json({ message: "Internal Server Error." });
  }
});

export default router;