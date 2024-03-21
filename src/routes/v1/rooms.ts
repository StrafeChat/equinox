import { Router } from "express";
import { verifyToken } from "../../helpers/validator";
import rateLimit from "express-rate-limit";
import { Request } from "express";
import Room from "../../database/models/Room";
import { redis } from "../../database";
import { generateSnowflake } from "../../helpers/generator";
import { MESSAGE_WORKER_ID, ROOM_WORKER_ID } from "../../config";
import { IMessage, IRoom, MessageSudo } from "../../types";
import Message from "../../database/models/Message";
import Space from "../../database/models/Space";

const router = Router();

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

router.post(
  "/",
  verifyToken,
  roomCreateLimit,
  async (req, res) => {
    const { space_id, name, type } = req.body;

    try {

      if (!name || !type) 
      return res.status(400).json({ message: "You need to provide a vaild name and type for the room. If you want it to be in a space then you must provide a vaild space_id." });

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
        return res
          .status(403)
          .json({ message: "You do not have permission to create rooms on this space." });

        switch (type) {
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
              parent_id: null,
              last_pin_timestamp: null,
              rtc_region: null
            };
    
            await Room.insert(TextRoom, { prepare: true });
            await Space.update({
              $where: [{ equals: ["id", space.id] }, { equals: ["created_at", space.created_at] }],
              $set: { room_ids: Array.from(roomIds) },
            })

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
                parent_id: null,
                last_pin_timestamp: null,
                rtc_region: null
              };
      
              await Room.insert(VoiceRoom, { prepare: true });
              await Space.update({
                $where: [{ equals: ["id", space.id] }, { equals: ["created_at", space.created_at] }],
                $set: { room_ids: Array.from(roomIds) },
              })
  
              res.status(200).json(VoiceRoom);
              break;
            default:
              return res
                .status(400)
                .json({ message: "This room does not support message sending." });
        }
      }
    } catch(err) {
      console.error(err);
      return res.status(500).json({ message: "Internal Server Error."})
    }
  })

  router.post(
    "/:room_id/messages",
    verifyToken,
    messageCreateLimit,
    async (req, res) => {
      const { room_id } = req.params;
  
      if (isNaN(parseInt(room_id)))
        return res.status(400).json({ message: "Invalid room ID." });
  
      const { content, message_reference_id, embeds, sudo } = req.body;
  
      if (!content && !embeds) return res.status(400).json({ message: "You must provide content for your message."});
  
      if (content && typeof content !== "string")
        return res
          .status(400)
          .json({ message: "Message content must be a string." });
      if (content && content.length > 1024)
        return res.status(400).json({
          message: "Message content must be less than 1,024 characters long.",
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
  
      const message_id = generateSnowflake(MESSAGE_WORKER_ID);
  
      switch (room.type) {
        case 1:
          const message: IMessage = {
            room_id,
            content,
            id: message_id,
            author_id: res.locals.user.id,
            space_id: room.space_id!,
            system: res.locals.user.system,
            tts: false,
            attachments: [],
            embeds: embeds || [],
            flags: 0,
            mention_everyone: false,
            mention_roles: [],
            mention_rooms: [],
            mentions: [],
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
  
          await Message.insert(message, { prepare: true });
  
          res.status(200).json({ ...message, nonce: 0 });
  
          await redis.publish(
            "stargate",
            JSON.stringify({
              event: "message_create",
              data: {
                room_type: room.type,
                space_id: room.space_id,
                room_id: room.id,
                content: content ?? null,
                embeds: embeds ?? [],
                sudo: sudo ?? null,
                message_reference_id: message_reference_id ?? null,
                id: message_id,
                created_at: message.created_at,
                author: {
                  id: res.locals.user.id,
                  username: res.locals.user.username,
                  discriminator: res.locals.user.discriminator,
                  global_name: res.locals.user.global_name,
                  display_name: res.locals.user.global_name ?? res.locals.user.username,
                  avatar: res.locals.user.avatar,
                  bot: res.locals.user.bot,
                  presence: res.locals.user.presence,
                  created_at: res.locals.user.created_at,
                }
              },
            })
          );
          break;
        default:
          return res
            .status(400)
            .json({ message: "This room does not support message sending." });
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
      
      if (!content && !embeds) return res.status(400).json({ message: "You must provide content for your message."});
  
      if (content && typeof content !== "string")
        return res
          .status(400)
          .json({ message: "Message content must be a string." });
      if (content && content.length > 1024)
        return res.status(400).json({
          message: "Message content must be less than 1,024 characters long.",
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
          return res
            .status(404)
            .json({ message: "The message you were looking for does not exist." });

            Message.update({
              $where: [{ equals: ["id", message.id] }, { equals: ["created_at", message.created_at] }, { equals: ["room_id", message.room_id] }],
              $prepare: true,
              $set: { content: content ?? null, embeds: embeds ?? [], edited_at: Date.now() },
            })

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
                    display_name: res.locals.user.global_name ?? res.locals.user.username,
                    discriminator: res.locals.user.discriminator,
                    global_name: res.locals.user.global_name,
                    avatar: res.locals.user.avatar,
                    bot: res.locals.user.bot,
                    presence: res.locals.user.presence,
                    created_at: res.locals.user.created_at,
                  }
                },
              })
            );
    
            return res.status(200).json({
              message: { ...message, content, author: {
                id: res.locals.user.id,
                username: res.locals.user.username,
                display_name: res.locals.user.global_name ?? res.locals.user.username,
                discriminator: res.locals.user.discriminator,
                global_name: res.locals.user.global_name,
                avatar: res.locals.user.avatar,
                bot: res.locals.user.bot,
                presence: res.locals.user.presence,
              }},
          })
    } catch(err) {
      console.error(err);
      res.status(500).json({ message: "Interal Server Error" });
    }
});

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
          return res
            .status(404)
            .json({ message: "The message you were looking for does not exist." });

        await Message.delete({
          $where: [{ equals: ["id", message.id] }, { equals: ["created_at", message.created_at] }, { equals: ["room_id", message.room_id] }],
          $prepare: true,
        })

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
                display_name: res.locals.user.global_name ?? res.locals.user.username,
                discriminator: res.locals.user.discriminator,
                global_name: res.locals.user.global_name,
                avatar: res.locals.user.avatar,
                bot: res.locals.user.bot,
              }
            },
          })
        );

        return res.status(202).send({
          status: "Sent"
      });

      } catch(err) {
        console.error(err);
        res.status(500).json({ message: "Interal Server Error" });
      }
})

router.post(
  "/:room_id/typing",
  verifyToken,
  async (req, res) => {
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
            display_name: res.locals.user.global_name ?? res.locals.user.username,
            discriminator: res.locals.user.discriminator,
            global_name: res.locals.user.global_name,
            avatar: res.locals.user.avatar,
            bot: res.locals.user.bot,
          }
        },
      })
    );

   return res.status(204).json({ message: "OK"});
    
    } catch(err) {
      console.error(err);
      res.status(500).json({ message: "Interal Server Error" });
    }
  }
)

export default router;
