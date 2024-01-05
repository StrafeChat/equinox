import utils from "log-utils";

export class Logger {
    public static start() {
        console.log(utils.timestamp, utils.heading('Starting Equinox...'));
    }
    public static log(message: string) {
        console.log(utils.timestamp, message);
    }
    public static error(message: string) {
        console.log(utils.timestamp, utils.error, message);
    }
    public static warn(message: string) {
        console.log(utils.timestamp, utils.warn, message);
    }
    public static info(message: string) {
        console.log(utils.timestamp, utils.info, message);
    }
    public static debug(message: string) {
        console.log(utils.timestamp, utils.debug, message);
    }
    public static success(message: string) {
        console.log(utils.timestamp, utils.success, message);
    }
}