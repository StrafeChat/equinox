import { Snowflake } from "../util/Snowflake"

const snowflakes = new Map<number, Snowflake>();

export const generateSnowflake = (workerId: number) => {
    if (!snowflakes.has(workerId)) snowflakes.set(workerId, new Snowflake(parseInt(process.env.SNOWFLAKE_EPOCH!, 10), workerId));
    return snowflakes.get(workerId)!.generate();
}