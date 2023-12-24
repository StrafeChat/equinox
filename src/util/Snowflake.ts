export class Snowflake {
    private inc: number;
    private lastTimestamp: BigInt;
    private epoch: number;
    private workerId: number;

    private readonly TIMESTAMP_SHIFT = BigInt(12);
    private readonly CURRENT_TIMESTAMP_SHIFT = BigInt(22);
    private readonly WORKER_ID_SHIFT = BigInt(17);
    private readonly SEQUENCE_MASK = 0b111111111111;

    constructor(epoch: number, workerId: number) {
        this.inc = 0;
        this.lastTimestamp = BigInt(-1);
        this.epoch = epoch;
        this.workerId = workerId & 0b11111;
    }

    public generate(): string {
        const currentTimestamp = this.getCurrentTimestamp();

        if (currentTimestamp === this.lastTimestamp) {
            this.inc = (this.inc + 1) & this.SEQUENCE_MASK; // 12-bit sequence number rollover
            if (this.inc === 0) {
                // Maximum retries have occured so we should throw an error.
                throw new Error('Sequence overflow');
            }
        } else {
            this.inc = 0;
            this.lastTimestamp = currentTimestamp;
        }

        const snowflake = BigInt(currentTimestamp) << this.CURRENT_TIMESTAMP_SHIFT | BigInt(this.workerId) << this.WORKER_ID_SHIFT | BigInt(this.inc);
        return snowflake.toString();
    }

    private getCurrentTimestamp() {
        return BigInt(new Date().getTime() - this.epoch) >> this.TIMESTAMP_SHIFT;
    }
}