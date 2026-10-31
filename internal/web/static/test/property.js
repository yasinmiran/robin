// Random and mutated inputs for the property tests of the page's parsers,
// from a seeded generator so a failure can be replayed: each failure names
// its seed, and ROBIN_PROPERTY_SEED runs that one. ROBIN_PROPERTY_RUNS sets
// how many inputs each property takes; make fuzz raises it.

export const RUNS = Number(process.env.ROBIN_PROPERTY_RUNS) || 500;
const SEED = process.env.ROBIN_PROPERTY_SEED;

// Rand is mulberry32: small, fast and good enough to pick test inputs.
export class Rand {
  #s;

  constructor(seed) {
    this.seed = seed >>> 0;
    this.#s = this.seed;
  }

  // u32 is the next 32 random bits.
  u32() {
    let t = (this.#s = (this.#s + 0x6d2b79f5) >>> 0);
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return (t ^ (t >>> 14)) >>> 0;
  }

  // int is an integer from 0 to n - 1.
  int(n) {
    return n <= 0 ? 0 : this.u32() % n;
  }

  bool() {
    return (this.u32() & 1) === 1;
  }

  pick(list) {
    return list[this.int(list.length)];
  }

  bytes(n) {
    const out = new Uint8Array(n);
    for (let i = 0; i < n; i++) out[i] = this.u32() & 0xff;
    return out;
  }

  // edge is a number a parser may get wrong.
  edge() {
    return this.pick([0, 1, -1, 0.5, 63, 64, 65, 255, 256, 65535, 65536, 0x7fffffff, 0xffffffff, 2 ** 32, 2 ** 53, -(2 ** 53), NaN, Infinity, -Infinity, 1e400]);
  }

  // value is any JSON value, nested at most depth deep.
  value(depth = 3) {
    switch (this.int(depth > 0 ? 7 : 5)) {
      case 0: return null;
      case 1: return this.bool();
      case 2: return this.edge();
      case 3: return this.string();
      case 4: return this.int(1000);
      case 5: return Array.from({ length: this.int(5) }, () => this.value(depth - 1));
      default: {
        const o = {};
        for (let i = this.int(5); i > 0; i--) o[this.string(8)] = this.value(depth - 1);
        return o;
      }
    }
  }

  // string mixes ASCII, the names objects already have, and code units a
  // decoder may stumble on.
  string(max = 16) {
    const special = ['__proto__', 'constructor', 'toString', 'hasOwnProperty', 't', 'header', 'end', '\u0000', '\ud800', '‮', '\n'];
    if (this.int(4) === 0) return this.pick(special);
    let s = '';
    for (let i = this.int(max); i > 0; i--) s += String.fromCharCode(this.int(4) === 0 ? this.u32() & 0xffff : 0x20 + this.int(95));
    return s;
  }

  // mutate returns a copy of bytes with a few random edits: bits flipped,
  // bytes set to edge values, inserted, deleted, cut off or repeated.
  mutate(bytes) {
    let b = [...bytes];
    for (let n = 1 + this.int(4); n > 0; n--) {
      const at = this.int(b.length + 1);
      switch (this.int(6)) {
        case 0: if (b.length) b[at % b.length] ^= 1 << this.int(8); break;
        case 1: if (b.length) b[at % b.length] = this.pick([0, 1, 0x7f, 0x80, 0xff]); break;
        case 2: b.splice(at, 0, ...this.bytes(1 + this.int(8))); break;
        case 3: b.splice(at, 1 + this.int(8)); break;
        case 4: b = b.slice(0, at); break;
        default: b.splice(at, 0, ...b.slice(at, at + 1 + this.int(16))); break;
      }
    }
    return new Uint8Array(b);
  }
}

// forAll runs property on RUNS generators, each of its own seed, and
// fails naming the seed that broke it.
export function forAll(property) {
  const seeds = SEED !== undefined ? [Number(SEED)] : Array.from({ length: RUNS }, (_, i) => (Date.now() + i * 0x9e3779b9) >>> 0);
  for (const seed of seeds) {
    try {
      property(new Rand(seed));
    } catch (e) {
      throw new Error(`${e.message}\n(ROBIN_PROPERTY_SEED=${seed} replays this)`, { cause: e });
    }
  }
}

// refusedOr runs f and returns { value } with what it returned, or
// { refused } with the Error it threw: the one way a parser may refuse.
// A TypeError or a RangeError is a parser reading past what it checked,
// and is thrown on.
export function refusedOr(f) {
  try {
    return { value: f() };
  } catch (e) {
    if (!(e instanceof Error) || e instanceof TypeError || e instanceof RangeError) throw e;
    return { refused: e };
  }
}
