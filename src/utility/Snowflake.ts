export class Snowflake {
    private inc: number;
    private lastSnowflake: string;
    private epoch: number;
  
    constructor() {
      this.inc = 0;
      this.lastSnowflake = "";
      this.epoch = 1697666079694;
    }
  
    public generate() {
      const pad = (num: number, by: number) => num.toString(2).padStart(by, "0");
  
      const msSince = pad(new Date().getTime() - this.epoch, 42);
      const pid = pad(process.pid, 5).slice(0, 5);
      const wid = pad(0 ?? 0, 5);
      const getInc = (add: number) => pad(this.inc + add, 12);
  
      let snowflake = `0b${msSince}${wid}${pid}${getInc(this.inc)}`;
      snowflake === this.lastSnowflake
        ? (snowflake = `0b${msSince}${wid}${pid}${getInc(++this.inc)}`)
        : (this.inc = 0);
  
      this.lastSnowflake = snowflake;
      return BigInt(snowflake).toString();
    }
  }
  