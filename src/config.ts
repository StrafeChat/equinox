export const ErrorCodes = {
    INTERNAL_SERVER_ERROR: {
        CODE: 500,
        MESSAGE: "An internal server error has occured. Please try again later."
    },
    EMAIL_ALREADY_EXISTS: {
        CODE: 403,
        MESSAGE: "A user with this email already exists."
    },
    USERNAME_AND_DISCRIMINATOR_TAKEN: {
        CODE: 403,
        MESSAGE: "A user already has that username and discriminator."
    }
}