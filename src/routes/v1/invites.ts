import { BatchDelete, BatchInsert, BatchUpdate } from "better-cassandra";
import { Router } from "express";
import { cassandra } from "../../database";
import { verifyToken } from "../../helpers/validator";
import rateLimit from "express-rate-limit";
import { Request } from "express";
import Room from "../../database/models/Room";
import Space from "../../database/models/Space";
import SpaceMember from "../../database/models/SpaceMember";
import Invite from "../../database/models/Invite";
import { ISpaceMember, IUser } from "../../types";

const router = Router();

router.get(
  "/:invite_code",
   verifyToken,
    async (req, res) => {
     const { invite_code } = req.params;
      
     try {
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
                $where: [{ equals: ["space_id", space.id] }]
              });

          return res.status(200).json({ 
            code: invite.code,
            inviter_id: invite.inviter_id,
            expires_at: invite.expires_at,
            room_id: invite.room_id,
            space_id: invite.space_id,
            uses: invite.uses,
            vanity: invite.vanity,
            created_at: Number(invite.created_at),
            member_count: member_count.length,
            space: { 
            id: invite.space_id,
            name: space.name,
            icon: space.icon,
            name_acronym: space.name_acronym,
            banner: space.banner,
            description: space.description,
          }
        });

     } catch (err) {
        console.log("ERROR: "+ err);
        return res.status(500).json({ message: "Interal Server Error."})
      }
   }
)

router.post(
  "/:invite_code",
  verifyToken,
   async (req, res) => {
    const { invite_code } = req.params;

      try {
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

            if(spaces.length < 1) return res.status(404).json({message: "The space that this invite is for was not found."})
        
            const space = spaces[0];

            if (res.locals.user.space_count >= 50) return res.status(403).json({ message: "You have reached the max amount of spaces you can join." });
        
            const members = await SpaceMember.select({
                $where: [{equals: ["user_id", res.locals.user.id]}, {equals: ["space_id", space.id]}]
            });
        
            if (members.length > 0) return res.status(409).json({message: "You are already in this space"});
        
            await cassandra.batch([
                BatchInsert<ISpaceMember>({
                    name: "space_members", data: {
                        user_id: res.locals.user.id,
                        space_id: space.id,
                        roles: [],
                        joined_at: Date.now(),
                        deaf: false,
                        mute: false,
                        avatar: null,
                    }
                }),
                BatchUpdate<IUser>({
                    name: "users",
                    set: {
                        space_ids: [...(res.locals.user.space_ids || []), space.id!]
                    },
                    where: [{equals: ["id", res.locals.user.id]}]
                })
            ]);
        
            const rooms = await Room.select({
                $where: [{ in: ["id", space.room_ids!] }],
              });
        
            return res.status(200).json({ space, rooms });

     } catch(err) {
        console.log("ERROR: "+ err);
        return res.status(500).json({ message: "Interal Server Error."})
     }
  }
)

export default router;
