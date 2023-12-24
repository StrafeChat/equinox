export interface RegisterBody {
    email: string;
    global_name?: string;
    username: string;
    discriminator: number;
    password: string;
    dob: Date;
    locale: string;
};