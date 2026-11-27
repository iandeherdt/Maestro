// Helpers for texture and model generators: plain Node, no packages.
// A generator is a script in ArtGenerators/ that writes into
// Assets/Art/Generated/, the one folder where Maestro accepts binary files
// in a Unity project. Run it with node, then new-meta.sh for its output.
//
//   import { png, obj, noise, fbm, mix, clamp } from "../conventions/unity/art.mjs";
//   png("Assets/Art/Generated/Textures/Concrete.png", 512, 512, (x, y) => {
//     const n = fbm(x / 64, y / 64, 5, 7);
//     const v = 90 + 60 * n;
//     return [v, v, v * 1.05, 255];
//   });
import { deflateSync } from "node:zlib";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

const crcTable = new Uint32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const out = Buffer.alloc(12 + data.length);
  out.writeUInt32BE(data.length, 0);
  out.write(type, 4, "ascii");
  data.copy(out, 8);
  out.writeUInt32BE(crc32(out.subarray(4, 8 + data.length)), 8 + data.length);
  return out;
}

export const clamp = (v, lo = 0, hi = 255) => Math.min(hi, Math.max(lo, v));
export const mix = (a, b, t) => a + (b - a) * t;

// png writes a w by h RGBA picture. pixel(x, y) returns [r, g, b, a] in
// 0..255 (a may be left out). Tileable textures: make pixel wrap at w and h.
export function png(path, w, h, pixel) {
  if (!(w >= 1 && h >= 1 && w <= 4096 && h <= 4096)) throw new Error(`png: size ${w}x${h}`);
  const raw = Buffer.alloc((w * 4 + 1) * h);
  let i = 0;
  for (let y = 0; y < h; y++) {
    raw[i++] = 0;
    for (let x = 0; x < w; x++) {
      const p = pixel(x, y);
      raw[i++] = clamp(Math.round(p[0]));
      raw[i++] = clamp(Math.round(p[1]));
      raw[i++] = clamp(Math.round(p[2]));
      raw[i++] = clamp(Math.round(p[3] ?? 255));
    }
  }
  const head = Buffer.alloc(13);
  head.writeUInt32BE(w, 0);
  head.writeUInt32BE(h, 4);
  head[8] = 8; // bits per channel
  head[9] = 6; // RGBA
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", head),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]));
}

// normalFromHeight turns a height function (0..1) into the pixel function
// of a tangent-space normal map. strength scales the slopes.
export function normalFromHeight(height, strength = 2) {
  return (x, y) => {
    const dx = (height(x + 1, y) - height(x - 1, y)) * strength;
    const dy = (height(x, y + 1) - height(x, y - 1)) * strength;
    const len = Math.hypot(dx, dy, 1);
    return [(-dx / len * 0.5 + 0.5) * 255, (dy / len * 0.5 + 0.5) * 255, (1 / len * 0.5 + 0.5) * 255, 255];
  };
}

// obj writes a Wavefront model: vertices [[x, y, z]], faces as lists of
// vertex indexes from 0, optional uvs [[u, v]] in the same order as the
// vertices. Unity reads it with Y up and metres as units.
export function obj(path, { vertices, faces, uvs, name = "Mesh" }) {
  const lines = [`o ${name}`];
  for (const v of vertices) lines.push(`v ${v.map((n) => +n.toFixed(5)).join(" ")}`);
  if (uvs) for (const t of uvs) lines.push(`vt ${t.map((n) => +n.toFixed(5)).join(" ")}`);
  for (const f of faces) lines.push("f " + f.map((i) => (uvs ? `${i + 1}/${i + 1}` : `${i + 1}`)).join(" "));
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, lines.join("\n") + "\n");
}

// Value noise that repeats every `period` cells, so textures tile; the
// same seed gives the same texture, so a generator can be run again.
function hash(x, y, seed) {
  let h = (x * 374761393 + y * 668265263 + seed * 144269504) | 0;
  h = Math.imul(h ^ (h >>> 13), 1274126177);
  return ((h ^ (h >>> 16)) >>> 0) / 4294967295;
}

export function noise(x, y, period = 256, seed = 1) {
  const xi = Math.floor(x), yi = Math.floor(y);
  const xf = x - xi, yf = y - yi;
  const m = (n) => ((n % period) + period) % period;
  const s = (t) => t * t * (3 - 2 * t);
  const a = hash(m(xi), m(yi), seed), b = hash(m(xi + 1), m(yi), seed);
  const c = hash(m(xi), m(yi + 1), seed), d = hash(m(xi + 1), m(yi + 1), seed);
  return mix(mix(a, b, s(xf)), mix(c, d, s(xf)), s(yf));
}

// fbm adds octaves of noise; 0..1. period is that of the first octave.
export function fbm(x, y, octaves = 4, period = 8, seed = 1) {
  let sum = 0, amp = 0.5, norm = 0;
  for (let o = 0; o < octaves; o++) {
    const f = 2 ** o;
    sum += amp * noise(x * f, y * f, period * f, seed + o);
    norm += amp;
    amp /= 2;
  }
  return sum / norm;
}
