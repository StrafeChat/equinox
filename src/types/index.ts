import { WebSocket } from "ws";

export interface UserWebSocket extends WebSocket {
    id: string;
    alive: boolean;
}

export interface RegisterBody {
    email: string;
    global_name?: string;
    username: string;
    discriminator: number;
    password: string;
    dob: Date;
    locale: string;
    captcha: string;
};

export interface LoginBody {
    email: string;
    password: string;
    captcha: string;
}

export interface IUserByEmail {
    email: string;
    id: string;
}

export interface IMessageByRoom {
    room_id: string;
    id: string;
}

export interface IUserByUsernameAndDiscriminator {
    discriminator: number;
    id: string;
    username: string;
}

export interface UserPresence {
    online: boolean;
    status: "online" | "offline" | "idle" | "dnd";
    status_text: string;
}

export interface IUser {
    id: string;
    accent_color: number;
    avatar: string;
    avatar_decoration: string;
    about_me: string;
    banned: boolean;
    banner: string;
    bot: boolean;
    created_at: Date | number;
    created_spaces_count: number;
    discriminator: number;
    dob: Date | number;
    edited_at: Date | number;
    email: string;
    flags: number;
    global_name: string;
    last_pass_reset: Date;
    locale: string;
    mfa_enabled: boolean;
    password: string;
    premium_type: number;
    presence: UserPresence;
    public_flags: number;
    secret: string;
    space_count: number;
    space_ids: string[];
    system: boolean;
    theme: string;
    username: string;
    verified: boolean;
}

export interface IVerification {
    id: string;
    code: string;
}

export interface PermissionOverwrite {
    id: string;
    type: number;
    allow_flags: number;
    deny_flags: number;
}

export interface IRoom {
    id: string;
    type: number;
    space_id: string | null;
    position: number;
    owner_id: string | null;
    permission_overwrites: PermissionOverwrite[],
    name: string | null;
    topic: string | null;
    last_message_id: string | null;
    bitrate: number | null;
    user_limit: number | null;
    rate_limit: number | null;
    recipients: string[];
    icon: string | null;
    parent_id: string | null;
    last_pin_timestamp: string | null;
    rtc_region: number | null;
    created_at: number;
    edited_at: number;
}

export interface ISpace {
    id: string;
    name: string;
    name_acronym: string;
    icon: string | null;
    owner_id: string;
    afk_room_id: string;
    afk_timeout: number;
    verifcation_level: number;
    room_ids: string[];
    role_ids: string[];
    rules_room_id: string;
    description: string;
    banner: string;
    preferred_locale: string;
    sticker_ids: string[];
    emoji_ids: string[];
    created_at: number;
    edited_at: number;
}

export interface IInvite {
    id: string;
    code: string;
    vanity: boolean;
    inviter_id: string;
    space_id: string;
    created_at: number;
    expires_at: number;
}

export interface ISpaceMember {
    user_id: string;
    space_id: string;
    nick: string | null;
    roles: string[];
    joined_at: number;
    deaf: boolean;
    mute: boolean;
    avatar: string | null;
    edited_at: number;
} 

export interface MessageEmbedFooter {
    text: string;
    icon_url: string | null;
}

export interface MessageEmbedAuthor {
    name: string | null;
    url: string | null;
    icon_url: string | null;
}

export interface MessageEmbedMedia {
    url: string;
    height: number | null;
    width: number | null;
}

export interface MessageEmbedField {
    name: string;
    value: string;
    inline: boolean;
}

export interface MessageEmbed {
    title: string | null;
    description: string | null;
    url: string | null;
    timestamp: number | null;
    color: string | null;
    footer: MessageEmbedFooter | null;
    image: MessageEmbedMedia | null;
    thumbnail: MessageEmbedMedia | null;
    video: MessageEmbedMedia | null;
    author: MessageEmbedAuthor | null;
    fields: MessageEmbedField[] | null;
}

export interface MessageReaction {
    user_ids: string[];
    emoji: string; 
}

export interface MessageSudo {
    name: string | null;
    avatar_url: string | null;
    color: string | null;
}


export interface IMessage {
    id: string;
    room_id: string;
    author_id: string;
    space_id: string | null;
    content: string | null;
    created_at: number;
    edited_at: number | null;
    tts: boolean;
    mention_everyone: boolean;
    mentions: string[] | null;
    mention_roles: string[] | null;
    mention_rooms: string[] | null;
    attachments: string[] | null;
    embeds: MessageEmbed[] | null;
    reactions: MessageReaction[] | null;
    sudo: MessageSudo | null;
    pinned: boolean;
    webhook_id: string | null;
    system: boolean;
    message_reference_id: string | null;
    flags: number | null;
    thread_id: string | null;
    stickers: string[] | null;
    nonce?: number;
}