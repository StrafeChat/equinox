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

export interface UserPresence {
    online: boolean;
    status: string;
    status_text: string;
}

export interface User {
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
    last_pass_reset: Date | number;
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