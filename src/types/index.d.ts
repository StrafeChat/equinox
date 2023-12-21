import { User } from "../interfaces/User";

declare module "express-serve-static-core" {
    interface Request {
        user: User;
    }
}