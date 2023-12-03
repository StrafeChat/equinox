export interface User {
    id: string;
    username: string;
    discriminator: number;
    global_name: string;
    avatar: string;
    bot: boolean;
    system: boolean;
    mfa_enabled: boolean;
    banner: string;
    accent_color: number;
    locale: string;
    verified: boolean;
    email: string;
    password: string;
    flags: number;
    premium_type: number;
    public_flags: number;
    avatar_decoration: string;
    created_at: Date,
    edited_at: Date,
    hide: boolean;
    secret: string;
    last_pass_reset: Date,
    dob: Date;
}