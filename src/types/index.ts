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
    created_at: Date | number;
    email: string;
    id: string;
}

export interface IUserByUsernameAndDiscriminator {
    created_at: Date | number;
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
    banned: boolean;
    banner: string;
    bot: boolean;
    created_at: Date | number;
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
    system: boolean;
    theme: string;
    username: string;
    verified: boolean;
}

export interface IVerification {
    id: string;
    code: string;
    created_at: number;
}

export interface IRoom {
    id: string;
    created_at: number;
    name: string;
    permissions: string[];
}

export interface ISpace {
    id: string;
    created_at: number;
    name: string;
    icon: string;
    banner: string;
    owner_id: string;
    rooms: IRoom[];
}