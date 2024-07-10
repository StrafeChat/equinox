interface _BitflagsMapping<Flags> {
    flags: Flags
    of(...flags: (keyof Flags)[]): _BitflagsValue<Flags>
    fromFlags(flags: { [key in keyof Flags]?: boolean }): _BitflagsValue<Flags>
    fromValue(value: number | bigint): _BitflagsValue<Flags>
    empty(): _BitflagsValue<Flags>
    all(): _BitflagsValue<Flags>
  }
  
  interface _BitflagsValue<Flags> {
    value: bigint
    has(flag: keyof Flags): boolean
    add(...targets: (keyof Flags)[]): this
    remove(...targets: (keyof Flags)[]): this
    toggle(...targets: (keyof Flags)[]): this
    update(newFlags: { [key in keyof Flags]?: boolean }): this
    clear(): this
    copy(): this
    get isEmpty(): boolean
    toFlags(): { [key in keyof Flags]: boolean }
  }

export function generateBitflags<
  RawFlags extends { [key: string]: number | bigint | ((flags: { [key: string]: bigint }) => number | bigint) },
  Flags extends RawFlags & { [key: string]: bigint },
>(rawFlags: RawFlags): _BitflagsMapping<Flags> {
  const flags: Flags = {} as any
  for (const [key, value] of Object.entries(rawFlags)) {
    flags[key as keyof Flags] = BigInt(typeof value === "function" ? value(flags) : value) as any
  }

  class _BitflagsGeneratedBase {
    static readonly flags = flags
    value: bigint

    private constructor(value: number | bigint = 0) {
      this.value = BigInt(value)
    }

    static of(...flags: (keyof Flags)[]) {
      return new this(flags.reduce((acc, flag) => acc | this.flags[flag], 0n))
    }

    static fromFlags(flags: { [key in keyof Flags]?: boolean }) {
      const value = Object.entries(flags).reduce((acc, [key, value]) => {
        if (value) acc |= this.flags[key]
        return acc
      }, 0n)
      return new this(value)
    }

    static fromValue(value: number | bigint) {
      return new this(BigInt(value))
    }

    static empty() {
      return new this()
    }

    static all() {
      return new this(Object.values(flags).reduce((acc, value) => acc | value, 0n))
    }

    has(flag: keyof Flags) {
      return (this.value & flags[flag]) === flags[flag]
    }

    add(...targets: (keyof Flags)[]) {
      for (const flag of targets) this.value |= flags[flag]
      return this
    }

    remove(...targets: (keyof Flags)[]) {
      for (const flag of targets) this.value &= ~flags[flag] as unknown as bigint
      return this
    }

    toggle(...targets: (keyof Flags)[]) {
      for (const flag of targets) this.value ^= flags[flag]
      return this
    }

    update(newFlags: { [key in keyof Flags]?: boolean }) {
      for (const [key, value] of Object.entries(newFlags)) {
        if (value) this.add(key as keyof Flags)
        else this.remove(key as keyof Flags)
      }
      return this
    }

    intersect(other: _BitflagsValue<Flags>) {
      this.value &= other.value
      return this
    }

    clear() {
      this.value = 0n
      return this
    }

    copy() {
      return new _BitflagsGeneratedBase(this.value)
    }

    get isEmpty() {
      return this.value === 0n
    }

    toFlags(): { [key in keyof Flags]: boolean } {
      return Object.fromEntries(Object.keys(flags).map((key) => [key, this.has(key)])) as any
    }
  }
  return _BitflagsGeneratedBase
}

export const Permissions = generateBitflags({
    VIEW_ROOM: 1 << 0,
    VIEW_MESSAGE_HISTORY: 1 << 1,
    SEND_MESSAGES: 1 << 2,
    MANAGE_MESSAGES: 1 << 3,
    ATTACH_FILES: 1 << 4,
    SEND_EMBEDS: 1 << 5,
    ADD_REACTIONS: 1 << 6,
    USE_SUDO: 1 << 7,
    PIN_MESSAGES: 1 << 8,
    PUBLISH_MESSAGES: 1 << 9,
    MANAGE_ROOMS: 1 << 10,
    MANAGE_WEBHOOKS: 1 << 11,
    MANAGE_EMOJIS: 1 << 12,
    MANAGE_SPACES: 1 << 13,
    MANAGE_ROLES: 1 << 14,
    CREATE_INVITES: 1 << 15,
    MANAGE_INVITES: 1 << 16,
    USE_EXTERNAL_EMOJIS: 1 << 17,
    CHANGE_NICKNAME: 1 << 18,
    MANAGE_NICKNAMES: 1 << 19,
    TIMEOUT_MEMBERS: 1 << 20,
    KICK_MEMBERS: 1 << 21,
    BAN_MEMBERS: 1 << 22,
    BULK_DELETE_MESSAGES: 1 << 23,
    VIEW_AUDIT_LOG: 1 << 24,
    PRIVILEGED_MENTIONS: 1 << 25,
    CONNECT: 1 << 26,
    SPEAK: 1 << 27,
    MUTE_MEMBERS: 1 << 28,
    DEAFEN_MEMBERS: 1 << 29,
    ADMINISTRATOR: 1 << 30,

    DEFAULT: (flags) =>
      flags.VIEW_ROOM
      | flags.VIEW_MESSAGE_HISTORY
      | flags.SEND_MESSAGES
      | flags.ADD_REACTIONS
      | flags.ATTACH_FILES
      | flags.SEND_EMBEDS
      | flags.CREATE_INVITES
      | flags.USE_EXTERNAL_EMOJIS
      | flags.CHANGE_NICKNAME
      | flags.CONNECT
      | flags.SPEAK,
  })