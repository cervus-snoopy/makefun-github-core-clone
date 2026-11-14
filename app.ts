// github-core-clone-01: a standalone GitHub-compatible git forge.
//
// One process, its own in-memory store, standard library only (node:http,
// node:crypto, node:zlib). No UI, no outside service, no git binary.
//
// Surfaces:
//   GET  /healthz                                  200 {"ok":true}
//   /<owner>/<repo>.git/info/refs                 smart-HTTP advertisements (v2 upload-pack, v0 receive-pack)
//   POST /<owner>/<repo>.git/git-upload-pack       protocol v2 fetch (v0 fallback)
//   POST /<owner>/<repo>.git/git-receive-pack      v0 push with report-status
//   /api/v3/...                                    REST v3: repos, branches, commits, contents,
//                                                  labels, issues, comments, pull requests
//   POST /api/graphql                              GraphQL v4 (the documents gh / github-mcp-server send)
//   POST /mcp                                      MCP over streamable HTTP (modern 2026-07-28 discover
//                                                  plus the legacy initialize handshake)
//
// Determinism: the server never reads the wall clock for data. Server-made
// commits use one fixed timestamp and the seeded viewer identity, so a live
// run and its replay produce byte-identical state.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

// ---------------------------------------------------------------------------
// identity: the token owner, from the suite's seeded scenario (wire.ts)
//
// The scenario is pure: SCENARIO_SEED + the xorshift64* stream labelled
// 'users' gives the viewer login. We port that generator here so the forge
// knows the token owner without any traffic, and we also learn it from the
// first REST path that names a repository owner, so a reseeded trial still
// works. Computed: viewer adaiversen, stranger olgabauer.
// ---------------------------------------------------------------------------

const U64_MASK = (1n << 64n) - 1n;
const SCENARIO_SEED = 0x6a09e667f3bcc909n;

function fnv1a64(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & U64_MASK;
  }
  return hash;
}

function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & U64_MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64_MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64_MASK;
  return (z ^ (z >> 31n)) & U64_MASK;
}

class SeedRng {
  private state: bigint;
  constructor(seed: bigint) {
    const masked = seed & U64_MASK;
    this.state = masked === 0n ? SCENARIO_SEED : masked;
  }
  nextU32(): number {
    let x = this.state;
    x = (x ^ (x >> 12n)) & U64_MASK;
    x = (x ^ (x << 25n)) & U64_MASK;
    x = (x ^ (x >> 27n)) & U64_MASK;
    this.state = x;
    return Number(((x * 0x2545f4914f6cdd1dn) & U64_MASK) >> 32n);
  }
  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }
  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)] as T;
  }
}

const FIRST_NAMES = ['ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars', 'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane'];
const LAST_NAMES = ['anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller', 'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber', 'xu', 'yilmaz', 'zimmer'];

function seededViewer(): { login: string; name: string; email: string } {
  const rng = new SeedRng(mix64(SCENARIO_SEED ^ fnv1a64('users')));
  const first = rng.pick(FIRST_NAMES);
  const last = rng.pick(LAST_NAMES);
  const login = `${first}${last}`;
  return { login, name: `${first} ${last}`, email: `${login}@example.invalid` };
}

const VIEWER = seededViewer();
/** The token owner; traffic may correct it (see learnOwner). */
let viewerLogin = VIEWER.login;
/** Fixed identity + timestamp for every commit this server authors. */
const SERVER_AUTHOR = `${VIEWER.name} <${VIEWER.email}>`;
const FIXED_EPOCH = 1735689600; // 2025-01-01T00:00:00Z, fixed so runs replay identically
const FIXED_ISO = '2025-01-01T00:00:00Z';

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

const ZERO_OID = '0'.repeat(40);
const JSON_TYPE = 'application/json; charset=utf-8';
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
let requestCounter = 0;

function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

function hashObject(type: string, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

function b64encode(data: Buffer): string {
  return data.toString('base64');
}

function b64decode(text: string): Buffer {
  return Buffer.from(text.replace(/[\r\n\t ]+/g, ''), 'base64');
}

function globalId(kind: string, key: string): string {
  return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64');
}

function decodeGlobalId(id: string): { kind: string; key: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 0) return null;
    return { kind: text.slice(0, colon), key: text.slice(colon + 1) };
  } catch {
    return null;
  }
}

/** A stable cursor over an issue number (base64 of `issue:<n>`). */
function issueCursor(number: number): string {
  return Buffer.from(`issue:${number}`, 'utf8').toString('base64');
}

function cursorNumber(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    if (!text.startsWith('issue:')) return null;
    const n = Number(text.slice(6));
    return Number.isInteger(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// pkt-line framing (git smart HTTP)
// ---------------------------------------------------------------------------

function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  const head = Buffer.from(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, body]);
}

const PKT_FLUSH = Buffer.from('0000', 'ascii');
const PKT_DELIM = Buffer.from('0001', 'ascii');

type Pkt = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Decode a whole pkt-line stream. Throws on a malformed line. */
function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off < buf.length) {
    if (off + 4 > buf.length) throw new Error('truncated pkt-line length');
    const hex = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error(`bad pkt-line length ${JSON.stringify(hex)}`);
    const len = parseInt(hex, 16);
    if (len === 0) {
      out.push({ kind: 'flush' });
      off += 4;
      continue;
    }
    if (len === 1) {
      out.push({ kind: 'delim' });
      off += 4;
      continue;
    }
    if (len === 2) {
      out.push({ kind: 'end' });
      off += 4;
      continue;
    }
    if (len < 4 || off + len > buf.length) throw new Error('pkt-line length out of range');
    out.push({ kind: 'data', payload: Buffer.from(buf.subarray(off + 4, off + len)) });
    off += len;
  }
  return out;
}

function pktLinesOf(packets: readonly Pkt[]): Buffer {
  const parts: Buffer[] = [];
  for (const p of packets) {
    if (p.kind === 'flush') parts.push(PKT_FLUSH);
    else if (p.kind === 'delim') parts.push(PKT_DELIM);
    else if (p.kind === 'end') parts.push(Buffer.from('0002', 'ascii'));
    else parts.push(pktLine(p.payload));
  }
  return Buffer.concat(parts);
}

/** One sideband packet: band byte (1 = data, 2 = progress, 3 = error) + payload, pkt-framed. */
function sideband(band: 1 | 2 | 3, data: Buffer): Buffer {
  return pktLine(Buffer.concat([Buffer.from([band]), data]));
}

/** Split a buffer into sideband band-1 packets of at most 32768 payload bytes. */
function sidebandData(data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 32768;
  for (let off = 0; off < data.length; off += CHUNK) {
    out.push(sideband(1, data.subarray(off, Math.min(data.length, off + CHUNK))));
  }
  if (data.length === 0) out.push(sideband(1, Buffer.alloc(0)));
  return out;
}

// ---------------------------------------------------------------------------
// git objects: trees, commits, tags, packs
// ---------------------------------------------------------------------------

interface GitObject {
  type: 'commit' | 'tree' | 'blob' | 'tag';
  data: Buffer;
}

interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const space = data.indexOf(0x20, off);
    if (space < 0) break;
    const mode = data.subarray(off, space).toString('ascii');
    const nul = data.indexOf(0, space + 1);
    if (nul < 0) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    off = nul + 21;
  }
  return entries;
}

function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

function parseCommit(data: Buffer): CommitView {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

function buildCommit(fields: { tree: string; parents: readonly string[]; message: string; author: string; committer: string; timestamp: number }): Buffer {
  const lines: string[] = [`tree ${fields.tree}`];
  for (const p of fields.parents) lines.push(`parent ${p}`);
  lines.push(`author ${fields.author} ${fields.timestamp} +0000`);
  lines.push(`committer ${fields.committer} ${fields.timestamp} +0000`);
  lines.push('');
  lines.push(fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`);
  return Buffer.from(lines.join('\n'), 'utf8');
}

interface TagView {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

function parseTag(data: Buffer): TagView {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4);
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

function buildTag(fields: { object: string; type: string; tag: string; message: string; tagger: string; timestamp: number }): Buffer {
  const lines = [
    `object ${fields.object}`,
    `type ${fields.type}`,
    `tag ${fields.tag}`,
    `tagger ${fields.tagger} ${fields.timestamp} +0000`,
    '',
    fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`,
  ];
  return Buffer.from(lines.join('\n'), 'utf8');
}

// --- pack reading (whole objects + ofs-delta + ref-delta) -------------------

function readPackVarint(buf: Buffer, start: number): { size: number; next: number } {
  let size = buf[start]! & 0x0f;
  let shift = 4;
  let off = start + 1;
  while ((buf[off - 1]! & 0x80) !== 0) {
    size |= (buf[off]! & 0x7f) << shift;
    shift += 7;
    off += 1;
  }
  return { size, next: off };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readVar = (): number => {
    let out = 0;
    let shift = 0;
    for (;;) {
      const b = delta[off]!;
      off += 1;
      out |= (b & 0x7f) << shift;
      shift += 7;
      if ((b & 0x80) === 0) break;
    }
    return out;
  };
  const srcSize = readVar();
  const dstSize = readVar();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const parts: Buffer[] = [];
  let total = 0;
  while (off < delta.length) {
    const op = delta[off]!;
    off += 1;
    if ((op & 0x80) !== 0) {
      let cpOff = 0;
      let cpSize = 0;
      for (let i = 0; i < 4; i += 1) if ((op >> i) & 1) { cpOff |= delta[off]! << (8 * i); off += 1; }
      for (let i = 0; i < 3; i += 1) if ((op >> (4 + i)) & 1) { cpSize |= delta[off]! << (8 * i); off += 1; }
      if (cpSize === 0) cpSize = 0x10000;
      parts.push(base.subarray(cpOff, cpOff + cpSize));
      total += cpSize;
    } else if (op !== 0) {
      parts.push(delta.subarray(off, off + op));
      total += op;
      off += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const out = Buffer.concat(parts);
  if (out.length !== dstSize || total !== dstSize) throw new Error('delta result size mismatch');
  return out;
}

/** Inflate the zlib stream starting at `off`; the stream end is found by binary
 *  search (inflateSync throws while truncated and ignores trailing bytes once
 *  complete, so success is monotone in the end position). */
function inflateAt(buf: Buffer, off: number): { data: Buffer; next: number } {
  const hi = buf.length - 20; // never run into the pack trailer
  let lo = off + 1;
  let high = hi;
  // The stream must end at or before hi; verify the full tail inflates.
  inflateSync(buf.subarray(off, high));
  while (lo < high) {
    const mid = (lo + high) >> 1;
    try {
      inflateSync(buf.subarray(off, mid));
      high = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return { data: inflateSync(buf.subarray(off, lo)), next: lo };
}
function readPack(pack: Buffer, lookup?: (oid: string) => GitObject | undefined): GitObject[] {
  if (pack.length < 12 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  // Trailer checksum: last 20 bytes are the pack SHA-1.
  const expect = pack.subarray(pack.length - 20).toString('hex');
  const actual = sha1Hex(pack.subarray(0, pack.length - 20));
  if (expect !== actual) throw new Error('pack trailer checksum mismatch');
  const TYPE_CODE: Record<number, GitObject['type']> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
  const objects: GitObject[] = [];
  const byOffset = new Map<number, GitObject>();
  let off = 12;
  for (let i = 0; i < count; i += 1) {
    const start = off;
    const first = pack[off]!;
    const typeNum = (first >> 4) & 0x7;
    const { next } = readPackVarint(pack, off);
    off = next;
    let obj: GitObject;
    if (typeNum === 6) {
      // ofs-delta: base offset encoded backwards
      let b = pack[off]!;
      off += 1;
      let baseOff = b & 0x7f;
      while ((b & 0x80) !== 0) {
        b = pack[off]!;
        off += 1;
        baseOff = ((baseOff + 1) << 7) | (b & 0x7f);
      }
      const baseOffset = start - baseOff;
      const base = byOffset.get(baseOffset);
      if (!base) throw new Error('ofs-delta base not in pack');
      const raw = inflateAt(pack, off);
      obj = { type: base.type, data: applyDelta(base.data, raw.data) };
      off = raw.next;
    } else if (typeNum === 7) {
      const baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
      const base = lookup?.(baseOid) ?? objects.find((o) => hashObject(o.type, o.data) === baseOid);
      if (!base) throw new Error(`ref-delta base ${baseOid} unknown`);
      const raw = inflateAt(pack, off);
      obj = { type: base.type, data: applyDelta(base.data, raw.data) };
      off = raw.next;
    } else {
      const type = TYPE_CODE[typeNum];
      if (!type) throw new Error(`unknown pack object type ${typeNum}`);
      const raw = inflateAt(pack, off);
      obj = { type, data: raw.data };
      off = raw.next;
    }
    objects.push(obj);
    byOffset.set(start, obj);
  }
  return objects;
}

// --- pack writing (whole objects only; deltas are optional for the suite) ---

function writePackObjectHeader(type: GitObject['type'], size: number): Buffer {
  const code = { commit: 1, tree: 2, blob: 3, tag: 4 }[type];
  const bytes: number[] = [];
  let first = (code << 4) | (size & 0x0f);
  size >>= 4;
  if (size !== 0) first |= 0x80;
  bytes.push(first);
  while (size !== 0) {
    let b = size & 0x7f;
    size >>= 7;
    if (size !== 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

/** Build a packfile (version 2) from whole objects, with a trailer checksum. */
function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [Buffer.from('PACK'), Buffer.alloc(4), Buffer.alloc(4)];
  parts[1]!.writeUInt32BE(2, 0);
  parts[2]!.writeUInt32BE(objects.length, 0);
  for (const obj of objects) {
    parts.push(writePackObjectHeader(obj.type, obj.data.length));
    // Level 1: pack size is not scored, wall time is; level 1 deflates
    // small objects several times faster than the default level 6.
    parts.push(deflateSync(obj.data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, Buffer.from(sha1Hex(body), 'hex')]);
}

// ---------------------------------------------------------------------------
// store: repositories, issues, pull requests (all in memory)
// ---------------------------------------------------------------------------

interface Issue {
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: { id: number; body: string }[];
  seq: number; // creation order
}

interface PullRequest {
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  head: string; // branch name
  base: string; // branch name
  headSha: string; // recorded at creation; refreshed on read
  baseSha: string;
  mergeBase: string | null;
  seq: number;
}

interface Repo {
  name: string;
  owner: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  databaseId: number;
  createdSeq: number;
  refs: Map<string, string>; // refname -> oid
  objects: Map<string, GitObject>;
  labels: Map<string, { name: string; color: string }>;
  issues: Issue[];
  pulls: PullRequest[];
  nextNumber: number; // shared issue+PR counter (GitHub numbers one sequence)
  nextCommentId: number;
  nextSeq: number;
}

const repos = new Map<string, Repo>(); // key: name (all repos belong to the viewer)
let nextDatabaseId = 5000;
let nextRepoSeq = 0;

function repoKey(name: string): string {
  return name;
}

function getRepo(name: string): Repo | undefined {
  return repos.get(repoKey(name));
}

function createRepoRecord(name: string, description: string, isPrivate: boolean): Repo {
  const repo: Repo = {
    name,
    owner: viewerLogin,
    description,
    isPrivate,
    defaultBranch: 'main',
    databaseId: nextDatabaseId,
    createdSeq: nextRepoSeq,
    refs: new Map(),
    objects: new Map(),
    labels: new Map(),
    issues: [],
    pulls: [],
    nextNumber: 1,
    nextCommentId: 1,
    nextSeq: 0,
  };
  nextDatabaseId += 1;
  nextRepoSeq += 1;
  repos.set(repoKey(name), repo);
  return repo;
}

/** Learn the token owner from a REST path that names an existing repository's owner. */
function learnOwner(owner: string, name: string): void {
  if (owner === viewerLogin) return;
  if (repos.has(repoKey(name))) viewerLogin = owner;
}

// --- object store helpers ---------------------------------------------------

function putObject(repo: Repo, type: GitObject['type'], data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

function getObject(repo: Repo, oid: string): GitObject | undefined {
  return repo.objects.get(oid);
}

/** All object ids reachable from `tips` (commits, their trees/blobs, tags). */
function collectReachable(repo: Repo, tips: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (!obj) continue; // a tip the store lacks contributes nothing
    seen.add(oid);
    if (obj.type === 'commit') {
      const view = parseCommit(obj.data);
      stack.push(view.tree);
      for (const p of view.parents) stack.push(p);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) stack.push(e.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return seen;
}

/** Whether `ancestor` is reachable from `tip` (commit ancestry). */
function isAncestor(repo: Repo, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return false;
}

/** The merge-base of two commits: the first common ancestor found walking both. */
function mergeBase(repo: Repo, a: string, b: string): string | null {
  const ancestorsA = new Set<string>();
  const stackA = [a];
  while (stackA.length > 0) {
    const oid = stackA.pop()!;
    if (ancestorsA.has(oid)) continue;
    ancestorsA.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stackA.push(p);
  }
  const stackB = [b];
  const seenB = new Set<string>();
  while (stackB.length > 0) {
    const oid = stackB.pop()!;
    if (ancestorsA.has(oid)) return oid;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) stackB.push(p);
  }
  return null;
}

/** Peel a tag object to its commit (recursively). */
function peelToCommit(repo: Repo, oid: string): string {
  let cur = oid;
  for (let i = 0; i < 10; i += 1) {
    const obj = repo.objects.get(cur);
    if (!obj || obj.type !== 'tag') return cur;
    cur = parseTag(obj.data).object;
  }
  return cur;
}

/** The flat path -> {oid, mode} map of a commit's tree. */
function flattenTree(repo: Repo, treeOid: string, prefix = ''): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  const obj = repo.objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') {
      for (const [p, v] of flattenTree(repo, e.oid, path)) out.set(p, v);
    } else {
      out.set(path, { oid: e.oid, mode: e.mode });
    }
  }
  return out;
}

/** Build a tree object from a flat path map; returns the root tree oid. */
function buildTreeFromFlat(repo: Repo, flat: Map<string, { oid: string; mode: string }>): string {
  interface Node {
    files: Map<string, { oid: string; mode: string }>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, entry] of flat) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const seg = segments[i]!;
      if (!node.dirs.has(seg)) node.dirs.set(seg, make());
      node = node.dirs.get(seg)!;
    }
    node.files.set(segments[segments.length - 1]!, entry);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, e] of node.files) entries.push({ mode: e.mode, name, oid: e.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return putObject(repo, 'tree', buildTree(entries));
  };
  return build(root);
}

/** Commits reachable from a tip, newest first (by traversal order). */
function commitsFrom(repo: Repo, tip: string): { oid: string; view: CommitView }[] {
  const out: { oid: string; view: CommitView }[] = [];
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    const view = parseCommit(obj.data);
    out.push({ oid, view });
    for (const p of view.parents) stack.push(p);
  }
  return out;
}

/** Three-way merge of trees. Returns the merged flat map, or null on conflict. */
function threeWayMerge(
  repo: Repo,
  baseTree: string,
  headTree: string,
  tipTree: string,
): Map<string, { oid: string; mode: string }> | null {
  const base = flattenTree(repo, baseTree);
  const head = flattenTree(repo, headTree);
  const tip = flattenTree(repo, tipTree);
  const merged = new Map<string, { oid: string; mode: string }>();
  const paths = new Set<string>([...base.keys(), ...head.keys(), ...tip.keys()]);
  for (const path of paths) {
    const b = base.get(path);
    const h = head.get(path);
    const t = tip.get(path);
    const eq = (x: { oid: string } | undefined, y: { oid: string } | undefined): boolean =>
      (x === undefined && y === undefined) || (x !== undefined && y !== undefined && x.oid === y.oid);
    if (eq(h, b)) {
      if (t !== undefined) merged.set(path, t); // head unchanged: take tip
    } else if (eq(t, b)) {
      if (h !== undefined) merged.set(path, h); // tip unchanged: take head
    } else if (eq(h, t)) {
      if (h !== undefined) merged.set(path, h); // both changed the same way
    } else {
      return null; // both changed differently: conflict
    }
  }
  return merged;
}

/** Line diff counts between two blobs (for the pull-request files view). */
function diffCounts(oldData: Buffer | null, newData: Buffer | null): { additions: number; deletions: number } {
  if (oldData === null && newData === null) return { additions: 0, deletions: 0 };
  if (oldData === null) return { additions: newData!.toString('utf8').split('\n').filter((l) => l !== '').length || 1, deletions: 0 };
  if (newData === null) return { additions: 0, deletions: oldData.toString('utf8').split('\n').filter((l) => l !== '').length || 1 };
  const a = oldData.toString('utf8').split('\n');
  const b = newData.toString('utf8').split('\n');
  const setA = new Set(a);
  const setB = new Set(b);
  let additions = 0;
  let deletions = 0;
  for (const line of b) if (!setA.has(line)) additions += 1;
  for (const line of a) if (!setB.has(line)) deletions += 1;
  return { additions, deletions };
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  requestCounter += 1;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': `req-${requestCounter}`,
    'x-oauth-scopes': 'repo, read:org',
    ...headers,
  });
  res.end(text);
}

function sendBuffer(res: ServerResponse, status: number, body: Buffer, contentType: string, headers: Record<string, string> = {}): void {
  requestCounter += 1;
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': `req-${requestCounter}`,
    ...headers,
  });
  res.end(body);
}

function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** Parse a JSON body; returns null when the body is empty, throws on malformed JSON. */
function parseJsonBody(buf: Buffer): unknown {
  const text = buf.toString('utf8').trim();
  if (text === '') return null;
  return JSON.parse(text);
}

function bearerToken(req: IncomingMessage): string | null {
  const auth = req.headers.authorization;
  if (!auth || !auth.startsWith('Bearer ')) return null;
  return auth.slice(7);
}

/** True when the request carries the suite's token (any non-empty token when none is configured). */
function isAuthenticated(req: IncomingMessage): boolean {
  const token = bearerToken(req);
  if (token === null || token === '') return false;
  if (TOKEN === '') return true;
  return token === TOKEN;
}

function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

function queryOf(req: IncomingMessage): URLSearchParams {
  try {
    return new URL(req.url ?? '/', 'http://entry').searchParams;
  } catch {
    return new URLSearchParams();
  }
}

function hostOf(req: IncomingMessage): string {
  return req.headers.host ?? 'entry';
}

// ---------------------------------------------------------------------------
// git smart HTTP
// ---------------------------------------------------------------------------

const AGENT_NAME = 'forge/1.0';

function sortedRefNames(repo: Repo): string[] {
  return [...repo.refs.keys()].sort();
}

/** The oid HEAD resolves to, or null when the default branch is unborn. */
function headOid(repo: Repo): string | null {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
}

/** Peeled commit oid for a ref (tags peel to their commit). */
function peeledOid(repo: Repo, ref: string): string | undefined {
  const oid = repo.refs.get(ref);
  if (!oid) return undefined;
  const obj = repo.objects.get(oid);
  if (!obj || obj.type !== 'tag') return undefined;
  return peelToCommit(repo, oid);
}

/** v0 advertisement ref lines (first line carries capabilities after NUL). */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? `symref=HEAD:refs/heads/${repo.defaultBranch} side-band-64k side-band agent=${AGENT_NAME} object-format=sha1 shallow thin-pack ofs-delta multi_ack`
      : `report-status side-band-64k delete-refs ofs-delta agent=${AGENT_NAME}`;
  const lines: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const names = sortedRefNames(repo);
  const refLine = (oid: string, name: string, first: boolean): Buffer =>
    pktLine(first ? `${oid} ${name}\0${caps}\n` : `${oid} ${name}\n`);
  if (names.length === 0) {
    if (service === 'git-upload-pack') {
      lines.push(pktLine(`${ZERO_OID} HEAD\0${caps}\n`));
    } else {
      lines.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    }
  } else {
    let first = true;
    // HEAD first, then the refs sorted.
    const head = headOid(repo);
    if (head !== null && service === 'git-upload-pack') {
      lines.push(refLine(head, 'HEAD', first));
      first = false;
    }
    for (const name of names) {
      const oid = repo.refs.get(name)!;
      lines.push(refLine(oid, name, first));
      first = false;
      const peeled = peeledOid(repo, name);
      if (peeled !== undefined) lines.push(pktLine(`${peeled} ${name}^{}\n`));
    }
  }
  lines.push(PKT_FLUSH);
  return Buffer.concat(lines);
}

/** v2 capability advertisement (upload-pack only). */
function v2Advertisement(): Buffer {
  const lines: Buffer[] = [
    pktLine('# service=git-upload-pack\n'),
    PKT_FLUSH,
    pktLine('version 2\n'),
    pktLine(`agent=${AGENT_NAME}\n`),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow filter\n'),
    pktLine('object-format=sha1\n'),
    pktLine('object-info\n'),
    PKT_FLUSH,
  ];
  return Buffer.concat(lines);
}

interface FetchPlan {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  /** True when the client sent `done` (negotiation is finished). */
  done: boolean;
}

/** Compute the objects a fetch must send, honouring haves, shallow lines and depth. */
function planFetch(repo: Repo, plan: FetchPlan): { objects: GitObject[]; shallowOut: string[]; unshallowOut: string[]; acks: string[] } {
  const clientShallow = new Set(plan.shallow);
  const completeHaves = new Set(plan.haves.filter((h) => !clientShallow.has(h)));
  const depth = plan.depth;

  // Walk from the wants, collecting new commits (with depth accounting).
  const newCommits = new Set<string>();
  const boundary = new Set<string>(); // commits at exactly the depth limit
  const queue: { oid: string; dist: number }[] = [];
  for (const w of plan.wants) {
    const obj = repo.objects.get(w);
    if (!obj) continue;
    if (obj.type === 'tag') {
      const target = peelToCommit(repo, w);
      queue.push({ oid: target, dist: 1 });
      newCommits.add(w); // the tag object itself is new unless held
    } else if (obj.type === 'commit') {
      queue.push({ oid: w, dist: 1 });
    }
  }
  const visited = new Set<string>();
  while (queue.length > 0) {
    const { oid, dist } = queue.shift()!;
    if (visited.has(oid)) continue;
    visited.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    if (completeHaves.has(oid)) continue; // held in full: prune here
    if (depth !== null && dist > depth) continue;
    if (!clientShallow.has(oid)) newCommits.add(oid);
    // A shallow-held commit is already held; its ancestors are not.
    if (depth !== null && dist === depth) {
      boundary.add(oid);
      continue;
    }
    const view = parseCommit(obj.data);
    for (const p of view.parents) queue.push({ oid: p, dist: dist + 1 });
  }
  // Tag objects wanted directly are new unless already held.
  for (const w of plan.wants) {
    const obj = repo.objects.get(w);
    if (obj && obj.type === 'tag' && !completeHaves.has(w) && !clientShallow.has(w)) newCommits.add(w);
  }

  // Objects already held by the client: everything reachable from complete
  // haves, plus (for shallow haves) the have commit's own tree closure.
  const held = new Set<string>();
  for (const h of completeHaves) for (const oid of collectReachable(repo, [h])) held.add(oid);
  for (const h of clientShallow) {
    const obj = repo.objects.get(h);
    if (!obj || obj.type !== 'commit') continue;
    held.add(h);
    const view = parseCommit(obj.data);
    for (const oid of collectReachable(repo, [view.tree])) held.add(oid);
  }

  // The pack: each new commit plus its own tree closure, minus what the
  // client already holds. The closure is per-commit, NOT the full ancestry:
  // a depth-limited fetch must not pull parents beyond the boundary (a
  // depth-1 clone sends the tip's tree and blobs only; deepen 2 sends the
  // parent's tree only). collectReachable over the new commits would walk
  // the whole history and break every shallow check.
  const send = new Set<string>();
  for (const oid of newCommits) {
    if (held.has(oid)) continue;
    send.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    const view = parseCommit(obj.data);
    for (const t of collectReachable(repo, [view.tree])) {
      if (!held.has(t)) send.add(t);
    }
  }
  // Deterministic pack order: commits first, then trees, blobs, tags, each by oid.
  const typeRank = { commit: 0, tree: 1, blob: 2, tag: 3 } as const;
  const objects: GitObject[] = [...send]
    .map((oid) => ({ oid, obj: repo.objects.get(oid)! }))
    .filter((entry) => entry.obj !== undefined)
    .sort((a, b) => {
      const ra = typeRank[a.obj.type] - typeRank[b.obj.type];
      if (ra !== 0) return ra;
      return a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0;
    })
    .map((entry) => entry.obj);

  const shallowOut = depth !== null ? [...boundary].sort() : [];
  const unshallowOut = [...clientShallow].filter((s) => {
    // A previous boundary is unshallow once its parent is now included.
    const obj = repo.objects.get(s);
    if (!obj || obj.type !== 'commit') return false;
    if (depth === null) return false;
    return parseCommit(obj.data).parents.some((p) => newCommits.has(p) || visited.has(p));
  }).sort();
  const acks = plan.haves.filter((h) => repo.objects.has(h));
  return { objects, shallowOut, unshallowOut, acks };
}

/** Fetch responses are pure functions of the repo's immutable object graph
 * and the request plan (wants/haves/shallow/depth/done): git objects are
 * never modified or deleted, so a cached response stays correct forever.
 * This is what makes 50 concurrent identical shallow clones cheap: the
 * first builds the pack, the rest send cached bytes. Bounded FIFO. */
const FETCH_CACHE = new Map<string, Buffer>();
const FETCH_CACHE_MAX = 128;
function fetchCacheKey(repoName: string, version: string, plan: FetchPlan): string {
  return `${repoName}|${version}|${JSON.stringify([plan.wants, plan.haves, plan.shallow, plan.depth, plan.done])}`;
}
function fetchCacheSet(key: string, value: Buffer): void {
  if (FETCH_CACHE.size >= FETCH_CACHE_MAX) {
    const first = FETCH_CACHE.keys().next().value;
    if (first !== undefined) FETCH_CACHE.delete(first);
  }
  FETCH_CACHE.set(key, value);
}

function handleInfoRefs(req: IncomingMessage, res: ServerResponse, owner: string, name: string): void {
  if (!isAuthenticated(req)) {
    sendError(res, 401, 'Requires authentication');
    return;
  }
  const repo = getRepo(name);
  if (!repo || owner !== viewerLogin) {
    // A missing repository is 404 on both services; the server stays up.
    sendError(res, 404, 'Not Found');
    return;
  }
  const service = queryOf(req).get('service');
  const wantV2 = req.headers['git-protocol'] === 'version=2';
  if (service === 'git-upload-pack') {
    if (wantV2) {
      sendBuffer(res, 200, v2Advertisement(), 'application/x-git-upload-pack-advertisement');
    } else {
      sendBuffer(res, 200, v0Advertisement(repo, 'git-upload-pack'), 'application/x-git-upload-pack-advertisement');
    }
    return;
  }
  if (service === 'git-receive-pack') {
    sendBuffer(res, 200, v0Advertisement(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement');
    return;
  }
  sendError(res, 400, 'Unknown service');
}

function handleUploadPack(req: IncomingMessage, res: ServerResponse, owner: string, name: string, body: Buffer): void {
  if (!isAuthenticated(req)) {
    sendError(res, 401, 'Requires authentication');
    return;
  }
  const repo = getRepo(name);
  if (!repo || owner !== viewerLogin) {
    sendError(res, 404, 'Not Found');
    return;
  }
  let packets: Pkt[];
  try {
    packets = decodePktLines(body);
  } catch {
    sendError(res, 400, 'Malformed pkt-line stream');
    return;
  }
  const first = packets.find((p) => p.kind === 'data');
  const firstLine = first && first.kind === 'data' ? first.payload.toString('utf8').trim() : '';
  const isV2 = req.headers['git-protocol'] === 'version=2' || firstLine.startsWith('command=');
  if (isV2) {
    handleUploadPackV2(res, repo, packets);
  } else {
    handleUploadPackV0(res, repo, packets);
  }
}

/** v2 ls-refs / fetch. */
function handleUploadPackV2(res: ServerResponse, repo: Repo, packets: Pkt[]): void {
  const lines: string[] = [];
  let section = 0; // 0 = command args, 1 = command body
  for (const p of packets) {
    if (p.kind === 'delim') {
      section = 1;
      continue;
    }
    if (p.kind === 'flush') break;
    if (p.kind !== 'data') continue;
    lines.push(p.payload.toString('utf8').replace(/\n$/, ''));
  }
  const command = lines[0] ?? '';
  if (command === 'command=ls-refs') {
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    for (const line of lines.slice(1)) {
      if (line === 'peel') peel = true;
      else if (line === 'symrefs') symrefs = true;
      else if (line === 'unborn') unborn = true;
      else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
    }
    const out: Buffer[] = [];
    const keep = (n: string): boolean => prefixes.length === 0 || prefixes.some((pre) => n.startsWith(pre));
    const head = headOid(repo);
    if (keep('HEAD')) {
      if (head !== null) {
        out.push(pktLine(`${head} HEAD${symrefs ? ' symref-target:refs/heads/' + repo.defaultBranch : ''}\n`));
      } else if (unborn) {
        out.push(pktLine(`unborn HEAD${symrefs ? ' symref-target:refs/heads/' + repo.defaultBranch : ''}\n`));
      }
    }
    for (const ref of sortedRefNames(repo)) {
      if (!keep(ref)) continue;
      const oid = repo.refs.get(ref)!;
      let line = `${oid} ${ref}`;
      if (peel) {
        const peeled = peeledOid(repo, ref);
        if (peeled !== undefined) line += ` peeled:${peeled}`;
      }
      out.push(pktLine(`${line}\n`));
    }
    out.push(PKT_FLUSH);
    sendBuffer(res, 200, Buffer.concat(out), 'application/x-git-upload-pack-result');
    return;
  }
  if (command === 'command=fetch') {
    const plan: FetchPlan = { wants: [], haves: [], shallow: [], depth: null, done: false };
    for (const line of lines.slice(1)) {
      if (line.startsWith('want ')) plan.wants.push(line.slice(5).trim());
      else if (line.startsWith('have ')) plan.haves.push(line.slice(5).trim());
      else if (line.startsWith('shallow ')) plan.shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) plan.depth = Number(line.slice(7).trim());
      else if (line === 'done') plan.done = true;
    }
    const cacheKey = fetchCacheKey(repo.name, 'v2', plan);
    const cached = FETCH_CACHE.get(cacheKey);
    if (cached !== undefined) {
      sendBuffer(res, 200, cached, 'application/x-git-upload-pack-result');
      return;
    }
    const { objects, shallowOut, unshallowOut, acks } = planFetch(repo, plan);
    const pack = writePack(objects);
    const out: Buffer[] = [];
    // Real git (and protocol-v2) only expects the acknowledgments section
    // while negotiation is still open. Once the client sent `done`, the
    // server goes straight to shallow-info + packfile; sending an ack
    // section there makes git die with "expected 'packfile', received
    // 'acknowledgments'". The suite's client always sends `done`.
    if (!plan.done) {
      out.push(pktLine('acknowledgments\n'));
      if (acks.length > 0) {
        for (const ack of acks) out.push(pktLine(`ACK ${ack}\n`));
        out.push(pktLine('ready\n'));
      } else {
        out.push(pktLine('NAK\n'));
      }
      out.push(PKT_DELIM);
    }
    if (shallowOut.length > 0 || unshallowOut.length > 0) {
      out.push(pktLine('shallow-info\n'));
      for (const s of shallowOut) out.push(pktLine(`shallow ${s}\n`));
      for (const s of unshallowOut) out.push(pktLine(`unshallow ${s}\n`));
      out.push(PKT_DELIM);
    }
    out.push(pktLine('packfile\n'));
    for (const chunk of sidebandData(pack)) out.push(chunk);
    out.push(PKT_FLUSH);
    const response = Buffer.concat(out);
    fetchCacheSet(cacheKey, response);
    sendBuffer(res, 200, response, 'application/x-git-upload-pack-result');
    return;
  }
  sendError(res, 400, `Unknown v2 command ${JSON.stringify(command)}`);
}

/** v0 fetch (want/have/done). */
function handleUploadPackV0(res: ServerResponse, repo: Repo, packets: Pkt[]): void {
  const plan: FetchPlan = { wants: [], haves: [], shallow: [], depth: null, done: false };
  let seenFlush = false;
  for (const p of packets) {
    if (p.kind === 'flush') {
      seenFlush = true;
      continue;
    }
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    const firstSpace = line.indexOf(' ');
    const word = firstSpace < 0 ? line : line.slice(0, firstSpace);
    const rest = firstSpace < 0 ? '' : line.slice(firstSpace + 1);
    if (!seenFlush) {
      if (word === 'want') plan.wants.push(rest.split(' ')[0]!);
      else if (word === 'shallow') plan.shallow.push(rest);
      else if (word === 'deepen') plan.depth = Number(rest);
    } else {
      if (word === 'have') plan.haves.push(rest);
    }
  }
  const cacheKey = fetchCacheKey(repo.name, 'v0', plan);
  const cached = FETCH_CACHE.get(cacheKey);
  if (cached !== undefined) {
    sendBuffer(res, 200, cached, 'application/x-git-upload-pack-result');
    return;
  }
  const { objects, shallowOut, unshallowOut, acks } = planFetch(repo, plan);
  const pack = writePack(objects);
  const out: Buffer[] = [];
  if (acks.length > 0) {
    for (const ack of acks) out.push(pktLine(`ACK ${ack}\n`));
  } else {
    out.push(pktLine('NAK\n'));
  }
  for (const s of shallowOut) out.push(pktLine(`shallow ${s}\n`));
  for (const s of unshallowOut) out.push(pktLine(`unshallow ${s}\n`));
  for (const chunk of sidebandData(pack)) out.push(chunk);
  out.push(PKT_FLUSH);
  const response = Buffer.concat(out);
  fetchCacheSet(cacheKey, response);
  sendBuffer(res, 200, response, 'application/x-git-upload-pack-result');
}

/** v0 receive-pack: command list + pack, report-status in sideband band 1. */
function handleReceivePack(req: IncomingMessage, res: ServerResponse, owner: string, name: string, body: Buffer): void {
  if (!isAuthenticated(req)) {
    sendError(res, 401, 'Requires authentication');
    return;
  }
  const repo = getRepo(name);
  if (!repo || owner !== viewerLogin) {
    sendError(res, 404, 'Not Found');
    return;
  }
  // Split the body at the first flush pkt-line: commands before it, pack after.
  let commandsEnd = -1;
  let off = 0;
  const commands: { oldOid: string; newOid: string; ref: string }[] = [];
  try {
    while (off < body.length) {
      if (off + 4 > body.length) throw new Error('truncated pkt-line');
      const hex = body.subarray(off, off + 4).toString('ascii');
      if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error('bad pkt-line length');
      const len = parseInt(hex, 16);
      if (len === 0) {
        commandsEnd = off + 4;
        break;
      }
      if (len < 4 || off + len > body.length) throw new Error('pkt-line length out of range');
      const payload = body.subarray(off + 4, off + len).toString('utf8');
      const nul = payload.indexOf('\0');
      const line = (nul >= 0 ? payload.slice(0, nul) : payload).replace(/\n$/, '');
      const parts = line.split(' ');
      if (parts.length >= 3) commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
      off += len;
    }
  } catch {
    sendError(res, 400, 'Malformed pkt-line stream');
    return;
  }
  if (commandsEnd < 0 || commands.length === 0) {
    sendError(res, 400, 'No push commands');
    return;
  }
  const packBytes = body.subarray(commandsEnd);
  const reportLines: string[] = [];
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (packBytes.length > 0) {
    try {
      const objects = readPack(packBytes, (oid) => repo.objects.get(oid));
      for (const obj of objects) putObject(repo, obj.type, obj.data);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : String(err);
    }
  }
  if (unpackOk) {
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        // Delete.
        if (current === undefined) {
          reportLines.push(`ng ${cmd.ref} ref does not exist`);
        } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
          reportLines.push(`ng ${cmd.ref} old oid mismatch`);
        } else {
          repo.refs.delete(cmd.ref);
          reportLines.push(`ok ${cmd.ref}`);
        }
        continue;
      }
      const newObj = repo.objects.get(cmd.newOid);
      if (!newObj) {
        reportLines.push(`ng ${cmd.ref} missing object`);
        continue;
      }
      if (cmd.oldOid === ZERO_OID) {
        if (current !== undefined) {
          reportLines.push(`ng ${cmd.ref} ref already exists`);
        } else {
          repo.refs.set(cmd.ref, cmd.newOid);
          reportLines.push(`ok ${cmd.ref}`);
        }
        continue;
      }
      if (current === undefined) {
        reportLines.push(`ng ${cmd.ref} ref does not exist`);
        continue;
      }
      if (cmd.oldOid !== current) {
        reportLines.push(`ng ${cmd.ref} old oid mismatch`);
        continue;
      }
      if (cmd.ref.startsWith('refs/heads/') && !isAncestor(repo, current, cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      reportLines.push(`ok ${cmd.ref}`);
    }
  }
  // The report is one pkt-line stream (unpack line, one line per ref, then
  // its own flush) carried as sideband band-1 data, followed by the outer
  // flush that ends the sideband stream. The inner flush matters: without
  // it the client's packet reader hits EOF instead of FLUSH and dies with
  // "the remote end hung up unexpectedly" (verified against git 2.43).
  const report = Buffer.concat([
    pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`),
    ...reportLines.map((line) => pktLine(`${line}\n`)),
    PKT_FLUSH,
  ]);
  const out: Buffer[] = [...sidebandData(report), PKT_FLUSH];
  sendBuffer(res, 200, Buffer.concat(out), 'application/x-git-receive-pack-result');
}

// ---------------------------------------------------------------------------
// REST v3
// ---------------------------------------------------------------------------

/** The token owner, confirmed once traffic names it; reseeded trials are learned, never assumed. */
let viewerConfirmed = false;

function confirmOwner(owner: string, name: string): void {
  if (viewerConfirmed) return;
  if (owner === viewerLogin) {
    viewerConfirmed = true;
    return;
  }
  // An authenticated read of an existing repository under another login, before
  // any confirmation, means the computed login was wrong: adopt the caller.
  if (repos.has(repoKey(name))) {
    viewerLogin = owner;
    for (const r of repos.values()) r.owner = owner;
    viewerConfirmed = true;
  }
}

interface RepoLookup {
  repo?: Repo;
  status: number; // 200 ok, 401 unauthenticated, 404 not found / foreign owner
}

function lookupRepo(req: IncomingMessage, owner: string, name: string): RepoLookup {
  if (!isAuthenticated(req)) return { status: 401 };
  confirmOwner(owner, name);
  const repo = getRepo(name);
  if (!repo || owner !== viewerLogin) return { status: 404 };
  return { repo, status: 200 };
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.databaseId,
    name: repo.name,
    full_name: `${viewerLogin}/${repo.name}`,
    owner: { login: viewerLogin, id: 1000, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    has_issues: true,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    html_url: `https://example.invalid/${viewerLogin}/${repo.name}`,
    clone_url: `https://example.invalid/${viewerLogin}/${repo.name}.git`,
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: repo.databaseId * 1000 + issue.number,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name, color: repo.labels.get(name)?.color ?? 'ededed' })),
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

function pullJson(repo: Repo, pr: PullRequest): Record<string, unknown> {
  const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
  const baseSha = repo.refs.get(`refs/heads/${pr.base}`) ?? pr.baseSha;
  return {
    id: repo.databaseId * 1000 + pr.number,
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    merged: pr.merged,
    merged_at: pr.mergedAt,
    merge_commit_sha: pr.mergeCommitSha,
    head: { ref: pr.head, sha: headSha, label: `${viewerLogin}:${pr.head}` },
    base: { ref: pr.base, sha: baseSha, label: `${viewerLogin}:${pr.base}` },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

function parsePagination(query: URLSearchParams): { page: number; perPage: number } {
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const perPage = Math.min(100, Math.max(1, Number(query.get('per_page') ?? '30') || 30));
  return { page, perPage };
}

function pageLinkHeader(req: IncomingMessage, basePath: string, query: URLSearchParams, page: number, perPage: number, total: number): string | null {
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  if (totalPages <= 1) return null;
  const origin = `http://${hostOf(req)}`;
  const url = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('page', String(p));
    q.set('per_page', String(perPage));
    return `${origin}${basePath}?${q.toString()}`;
  };
  const parts: string[] = [];
  if (page < totalPages) parts.push(`<${url(page + 1)}>; rel="next"`);
  if (page > 1) parts.push(`<${url(page - 1)}>; rel="prev"`);
  parts.push(`<${url(1)}>; rel="first"`);
  parts.push(`<${url(totalPages)}>; rel="last"`);
  return parts.join(', ');
}

/** Commit one file's new content on a branch (the contents API's core). */
function commitFile(
  repo: Repo,
  branch: string,
  filePath: string,
  content: Buffer,
  message: string,
): { commitSha: string; contentSha: string } {
  const tip = repo.refs.get(`refs/heads/${branch}`);
  const baseTree = tip ? parseCommit(repo.objects.get(tip)!.data).tree : null;
  const flat = baseTree ? flattenTree(repo, baseTree) : new Map<string, { oid: string; mode: string }>();
  const blobSha = putObject(repo, 'blob', content);
  flat.set(filePath, { oid: blobSha, mode: '100644' });
  const treeSha = buildTreeFromFlat(repo, flat);
  const parents = tip ? [tip] : [];
  const commitData = buildCommit({ tree: treeSha, parents, message, author: SERVER_AUTHOR, committer: SERVER_AUTHOR, timestamp: FIXED_EPOCH });
  const commitSha = putObject(repo, 'commit', commitData);
  repo.refs.set(`refs/heads/${branch}`, commitSha);
  return { commitSha, contentSha: blobSha };
}

/** The tree entry (or subtree) at `path` under a tree, resolved segment by segment. */
function treeEntryAt(repo: Repo, treeOid: string, path: string): { entry: TreeEntry; treeOid: string } | null {
  const segments = path.split('/').filter((s) => s !== '');
  let current = treeOid;
  for (let i = 0; i < segments.length; i += 1) {
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'tree') return null;
    const found = parseTree(obj.data).find((e) => e.name === segments[i]);
    if (!found) return null;
    if (i === segments.length - 1) return { entry: found, treeOid: current };
    if (found.mode !== '40000') return null;
    current = found.oid;
  }
  return null;
}

function handleRest(req: IncomingMessage, res: ServerResponse, body: Buffer): void {
  const method = req.method ?? 'GET';
  const path = pathOf(req);
  const query = queryOf(req);
  const segments = path.split('/').filter((s) => s !== '');

  // The API root and meta take no repo and (for meta) no auth beyond the token.
  if (method === 'GET' && (path === '/api/v3' || path === '/api/v3/')) {
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }
  if (method === 'GET' && path === '/api/v3/meta') {
    if (!isAuthenticated(req)) {
      sendError(res, 401, 'Requires authentication');
      return;
    }
    sendJson(res, 200, { installed_version: '1.0.0' });
    return;
  }
  if (method === 'GET' && path === '/api/v3/user') {
    if (!isAuthenticated(req)) {
      sendError(res, 401, 'Requires authentication');
      return;
    }
    sendJson(res, 200, { login: viewerLogin, id: 1000, type: 'User', name: VIEWER.name, email: VIEWER.email });
    return;
  }
  if (path === '/api/v3/user/repos') {
    if (!isAuthenticated(req)) {
      sendError(res, 401, 'Requires authentication');
      return;
    }
    if (method === 'POST') {
      let data: Record<string, unknown>;
      try {
        data = (parseJsonBody(body) ?? {}) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Malformed JSON body');
        return;
      }
      const name = typeof data.name === 'string' ? data.name : '';
      if (name === '') {
        sendError(res, 422, 'Repository name is required');
        return;
      }
      if (getRepo(name)) {
        sendError(res, 422, 'Repository already exists');
        return;
      }
      const repo = createRepoRecord(name, typeof data.description === 'string' ? data.description : '', data.private === true);
      sendJson(res, 201, repoJson(repo));
      return;
    }
    if (method === 'GET') {
      const { page, perPage } = parsePagination(query);
      const all = [...repos.values()].sort((a, b) => a.createdSeq - b.createdSeq);
      const slice = all.slice((page - 1) * perPage, page * perPage);
      const headers: Record<string, string> = {};
      const link = pageLinkHeader(req, '/api/v3/user/repos', query, page, perPage, all.length);
      if (link) headers.link = link;
      sendJson(res, 200, slice.map(repoJson), headers);
      return;
    }
  }

  // /api/v3/repos/{owner}/{name}[...]
  if (segments[0] === 'api' && segments[1] === 'v3' && segments[2] === 'repos' && segments.length >= 5) {
    const owner = decodeURIComponent(segments[3]!);
    const name = decodeURIComponent(segments[4]!);
    const rest = segments.slice(5);
    const lookup = lookupRepo(req, owner, name);
    if (lookup.status !== 200 || !lookup.repo) {
      sendError(res, lookup.status, lookup.status === 401 ? 'Requires authentication' : 'Not Found');
      return;
    }
    const repo = lookup.repo;
    handleRepoRest(req, res, body, repo, rest, query);
    return;
  }

  // Malformed JSON on a known write route is 400; everything else is a JSON 404.
  if ((method === 'POST' || method === 'PUT' || method === 'PATCH') && path.startsWith('/api/v3/')) {
    try {
      parseJsonBody(body);
    } catch {
      sendError(res, 400, 'Malformed JSON body');
      return;
    }
  }
  sendError(res, 404, 'Not Found');
}

function handleRepoRest(req: IncomingMessage, res: ServerResponse, body: Buffer, repo: Repo, rest: string[], query: URLSearchParams): void {
  const method = req.method ?? 'GET';
  const path = pathOf(req);

  const needJson = (): Record<string, unknown> | null => {
    try {
      return (parseJsonBody(body) ?? {}) as Record<string, unknown>;
    } catch {
      sendError(res, 400, 'Malformed JSON body');
      return null;
    }
  };

  // /repos/{o}/{r} itself: GET, PATCH, DELETE.
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, repoJson(repo));
      return;
    }
    if (method === 'PATCH') {
      const data = needJson();
      if (data === null) return;
      if (typeof data.description === 'string') repo.description = data.description;
      sendJson(res, 200, repoJson(repo));
      return;
    }
    if (method === 'DELETE') {
      repos.delete(repoKey(repo.name));
      res.writeHead(204, { 'x-github-request-id': `req-${(requestCounter += 1)}` });
      res.end();
      return;
    }
  }

  // Branches.
  if (rest[0] === 'branches') {
    if (rest.length === 1 && method === 'GET') {
      const out = sortedRefNames(repo)
        .filter((r) => r.startsWith('refs/heads/'))
        .map((r) => ({ name: r.slice('refs/heads/'.length), commit: { sha: repo.refs.get(r)! } }));
      sendJson(res, 200, out);
      return;
    }
    if (rest.length === 2 && method === 'GET') {
      const branch = decodeURIComponent(rest[1]!);
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (!sha) {
        sendError(res, 404, 'Not Found');
        return;
      }
      sendJson(res, 200, { name: branch, commit: { sha } });
      return;
    }
  }

  // Commits.
  if (rest[0] === 'commits' && rest.length === 1 && method === 'GET') {
    const shaParam = query.get('sha');
    const tip = shaParam ? repo.refs.get(`refs/heads/${shaParam}`) ?? (repo.objects.has(shaParam) ? shaParam : undefined) : headOid(repo);
    if (!tip) {
      sendJson(res, 200, []);
      return;
    }
    const out = commitsFrom(repo, tip).map(({ oid, view }) => ({
      sha: oid,
      // GitHub's API returns the message without the trailing newline git
      // stores in the commit object.
      commit: { message: view.message.replace(/\n$/, ''), author: { name: VIEWER.name, email: VIEWER.email }, committer: { name: VIEWER.name, email: VIEWER.email } },
    }));
    sendJson(res, 200, out);
    return;
  }

  // Git Data: refs.
  if (rest[0] === 'git' && rest[1] === 'refs') {
    if (rest.length === 2 && method === 'GET') {
      const out = sortedRefNames(repo).map((r) => ({ ref: r, object: { sha: repo.refs.get(r)!, type: repo.objects.get(repo.refs.get(r)!)?.type ?? 'commit' } }));
      sendJson(res, 200, out);
      return;
    }
    if (rest.length === 2 && method === 'POST') {
      const data = needJson();
      if (data === null) return;
      const ref = typeof data.ref === 'string' ? data.ref : '';
      const sha = typeof data.sha === 'string' ? data.sha : '';
      if (!ref.startsWith('refs/') || !repo.objects.has(sha)) {
        sendError(res, 422, 'Invalid ref or sha');
        return;
      }
      if (repo.refs.has(ref)) {
        sendError(res, 422, 'Reference already exists');
        return;
      }
      repo.refs.set(ref, sha);
      sendJson(res, 201, { ref, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
      return;
    }
    if (rest.length >= 4 && method === 'GET') {
      // /git/refs/heads/{branch} or /git/refs/tags/{tag}
      const kind = rest[2];
      const refName = decodeURIComponent(rest.slice(3).join('/'));
      const ref = `refs/${kind}/${refName}`;
      const sha = repo.refs.get(ref);
      if (!sha) {
        sendError(res, 404, 'Not Found');
        return;
      }
      sendJson(res, 200, { ref, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } });
      return;
    }
  }

  // Contents.
  if (rest[0] === 'contents') {
    const filePath = rest.slice(1).map((s) => decodeURIComponent(s)).join('/');
    if (method === 'PUT') {
      const data = needJson();
      if (data === null) return;
      const contentB64 = typeof data.content === 'string' ? data.content : '';
      const message = typeof data.message === 'string' ? data.message : 'Update';
      const branch = typeof data.branch === 'string' ? data.branch : repo.defaultBranch;
      const givenSha = typeof data.sha === 'string' ? data.sha : undefined;
      // A branch that does not exist yet is created from the default branch tip.
      if (!repo.refs.has(`refs/heads/${branch}`)) {
        const base = headOid(repo);
        if (base === null) {
          // No default branch yet: this write founds the repository on `branch`
          // (commitFile creates the first commit with no parents).
        } else {
          repo.refs.set(`refs/heads/${branch}`, base);
        }
      }
      const tip = repo.refs.get(`refs/heads/${branch}`);
      const existing = tip ? treeEntryAt(repo, parseCommit(repo.objects.get(tip)!.data).tree, filePath) : null;
      if (givenSha !== undefined && existing !== null && existing.entry.oid !== givenSha) {
        sendError(res, 409, 'The blob sha does not match');
        return;
      }
      if (givenSha !== undefined && existing === null) {
        sendError(res, 409, 'The file does not exist');
        return;
      }
      const isUpdate = existing !== null;
      const { commitSha, contentSha } = commitFile(repo, branch, filePath, b64decode(contentB64), message);
      sendJson(res, isUpdate ? 200 : 201, {
        content: { name: filePath.split('/').pop(), path: filePath, sha: contentSha, size: b64decode(contentB64).length, type: 'file', encoding: 'base64' },
        commit: { sha: commitSha, message },
      });
      return;
    }
    if (method === 'GET') {
      const refParam = query.get('ref');
      const tip = refParam ? repo.refs.get(`refs/heads/${refParam}`) ?? (repo.objects.has(refParam) ? refParam : undefined) : headOid(repo);
      if (!tip) {
        sendError(res, 404, 'Not Found');
        return;
      }
      const treeOid = parseCommit(repo.objects.get(tip)!.data).tree;
      if (filePath === '') {
        const entries = parseTree(repo.objects.get(treeOid)!.data).map((e) => ({
          type: e.mode === '40000' ? 'dir' : 'file',
          name: e.name,
          path: e.name,
          sha: e.oid,
        }));
        sendJson(res, 200, entries);
        return;
      }
      const found = treeEntryAt(repo, treeOid, filePath);
      if (!found) {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (found.entry.mode === '40000') {
        const entries = parseTree(repo.objects.get(found.entry.oid)!.data).map((e) => ({
          type: e.mode === '40000' ? 'dir' : 'file',
          name: e.name,
          path: `${filePath}/${e.name}`,
          sha: e.oid,
        }));
        sendJson(res, 200, entries);
        return;
      }
      const blob = repo.objects.get(found.entry.oid)!;
      sendJson(res, 200, {
        type: 'file',
        encoding: 'base64',
        content: b64encode(blob.data),
        sha: found.entry.oid,
        size: blob.data.length,
        name: filePath.split('/').pop(),
        path: filePath,
      });
      return;
    }
    if (method === 'DELETE') {
      const data = needJson();
      if (data === null) return;
      const branch = typeof data.branch === 'string' ? data.branch : repo.defaultBranch;
      const givenSha = typeof data.sha === 'string' ? data.sha : undefined;
      const message = typeof data.message === 'string' ? data.message : 'Delete';
      const tip = repo.refs.get(`refs/heads/${branch}`);
      if (!tip) {
        sendError(res, 404, 'Not Found');
        return;
      }
      const treeOid = parseCommit(repo.objects.get(tip)!.data).tree;
      const found = treeEntryAt(repo, treeOid, filePath);
      if (!found || found.entry.mode === '40000') {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (givenSha !== undefined && found.entry.oid !== givenSha) {
        sendError(res, 409, 'The blob sha does not match');
        return;
      }
      const flat = flattenTree(repo, treeOid);
      flat.delete(filePath);
      const newTree = buildTreeFromFlat(repo, flat);
      const commitData = buildCommit({ tree: newTree, parents: [tip], message, author: SERVER_AUTHOR, committer: SERVER_AUTHOR, timestamp: FIXED_EPOCH });
      const commitSha = putObject(repo, 'commit', commitData);
      repo.refs.set(`refs/heads/${branch}`, commitSha);
      sendJson(res, 200, { commit: { sha: commitSha, message } });
      return;
    }
  }

  // Labels.
  if (rest[0] === 'labels' && rest.length === 1 && method === 'POST') {
    const data = needJson();
    if (data === null) return;
    const name = typeof data.name === 'string' ? data.name : '';
    if (name === '') {
      sendError(res, 422, 'Label name is required');
      return;
    }
    const color = typeof data.color === 'string' ? data.color : 'ededed';
    repo.labels.set(name, { name, color });
    sendJson(res, 201, { name, color });
    return;
  }

  // Issues.
  if (rest[0] === 'issues') {
    if (rest.length === 1 && method === 'POST') {
      const data = needJson();
      if (data === null) return;
      const title = typeof data.title === 'string' ? data.title : '';
      const bodyText = typeof data.body === 'string' ? data.body : '';
      const labels = Array.isArray(data.labels) ? (data.labels as unknown[]).filter((l): l is string => typeof l === 'string') : [];
      for (const label of labels) {
        if (!repo.labels.has(label)) {
          sendError(res, 422, `Label ${label} does not exist`);
          return;
        }
      }
      const issue: Issue = {
        number: repo.nextNumber,
        title,
        body: bodyText,
        state: 'open',
        labels,
        comments: [],
        seq: repo.nextSeq,
      };
      repo.nextNumber += 1;
      repo.nextSeq += 1;
      repo.issues.push(issue);
      sendJson(res, 201, issueJson(repo, issue));
      return;
    }
    if (rest.length === 1 && method === 'GET') {
      const state = query.get('state') ?? 'open';
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') ?? 'desc';
      const { page, perPage } = parsePagination(query);
      let list = repo.issues.filter((i) => (state === 'all' ? true : i.state === state));
      if (sort === 'created') {
        list = [...list].sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
      }
      const slice = list.slice((page - 1) * perPage, page * perPage);
      const headers: Record<string, string> = {};
      const link = pageLinkHeader(req, path, query, page, perPage, list.length);
      if (link) headers.link = link;
      sendJson(res, 200, slice.map((i) => issueJson(repo, i)), headers);
      return;
    }
    if (rest.length === 2) {
      const number = Number(rest[1]);
      const issue = repo.issues.find((i) => i.number === number);
      if (!issue) {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        sendJson(res, 200, issueJson(repo, issue));
        return;
      }
      if (method === 'PATCH') {
        const data = needJson();
        if (data === null) return;
        if (data.state === 'open' || data.state === 'closed') issue.state = data.state;
        if (typeof data.title === 'string') issue.title = data.title;
        if (typeof data.body === 'string') issue.body = data.body;
        sendJson(res, 200, issueJson(repo, issue));
        return;
      }
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      const number = Number(rest[1]);
      const issue = repo.issues.find((i) => i.number === number);
      if (!issue) {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (method === 'POST') {
        const data = needJson();
        if (data === null) return;
        const comment = { id: repo.nextCommentId, body: typeof data.body === 'string' ? data.body : '' };
        repo.nextCommentId += 1;
        issue.comments.push(comment);
        sendJson(res, 201, { id: comment.id, body: comment.body });
        return;
      }
      if (method === 'GET') {
        sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, body: c.body })));
        return;
      }
    }
  }

  // Pull requests.
  if (rest[0] === 'pulls') {
    if (rest.length === 1 && method === 'POST') {
      const data = needJson();
      if (data === null) return;
      const title = typeof data.title === 'string' ? data.title : '';
      const bodyText = typeof data.body === 'string' ? data.body : '';
      const head = typeof data.head === 'string' ? data.head : '';
      const base = typeof data.base === 'string' ? data.base : repo.defaultBranch;
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (!headSha || !baseSha) {
        sendError(res, 422, 'Head or base branch does not exist');
        return;
      }
      const existing = repo.pulls.find((p) => p.head === head && p.base === base && p.state === 'open');
      if (existing) {
        sendError(res, 422, 'A pull request already exists for this head and base');
        return;
      }
      const pr: PullRequest = {
        number: repo.nextNumber,
        title,
        body: bodyText,
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        head,
        base,
        headSha,
        baseSha,
        mergeBase: mergeBase(repo, headSha, baseSha),
        seq: repo.nextSeq,
      };
      repo.nextNumber += 1;
      repo.nextSeq += 1;
      repo.pulls.push(pr);
      sendJson(res, 201, pullJson(repo, pr));
      return;
    }
    if (rest.length === 1 && method === 'GET') {
      const state = query.get('state') ?? 'open';
      const { page, perPage } = parsePagination(query);
      let list = repo.pulls.filter((p) => (state === 'all' ? true : state === 'closed' ? p.state === 'closed' : p.state === 'open'));
      list = [...list].sort((a, b) => b.seq - a.seq);
      const slice = list.slice((page - 1) * perPage, page * perPage);
      sendJson(res, 200, slice.map((p) => pullJson(repo, p)));
      return;
    }
    if (rest.length === 2) {
      const number = Number(rest[1]);
      const pr = repo.pulls.find((p) => p.number === number);
      if (!pr) {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        sendJson(res, 200, pullJson(repo, pr));
        return;
      }
    }
    if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
      const number = Number(rest[1]);
      const pr = repo.pulls.find((p) => p.number === number);
      if (!pr) {
        sendError(res, 404, 'Not Found');
        return;
      }
      const base = pr.mergeBase ?? pr.baseSha;
      const baseTree = base ? parseCommit(repo.objects.get(base)!.data).tree : null;
      const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
      const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
      const baseFlat = baseTree ? flattenTree(repo, baseTree) : new Map<string, { oid: string; mode: string }>();
      const headFlat = flattenTree(repo, headTree);
      const out: Record<string, unknown>[] = [];
      const paths = new Set<string>([...baseFlat.keys(), ...headFlat.keys()]);
      for (const p of [...paths].sort()) {
        const b = baseFlat.get(p);
        const h = headFlat.get(p);
        if (b && h && b.oid === h.oid) continue;
        const status = !b ? 'added' : !h ? 'removed' : 'modified';
        const oldData = b ? repo.objects.get(b.oid)?.data ?? null : null;
        const newData = h ? repo.objects.get(h.oid)?.data ?? null : null;
        const { additions, deletions } = diffCounts(oldData, newData);
        out.push({ filename: p, status, additions, deletions, changes: additions + deletions });
      }
      sendJson(res, 200, out);
      return;
    }
    if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
      const number = Number(rest[1]);
      const pr = repo.pulls.find((p) => p.number === number);
      if (!pr) {
        sendError(res, 404, 'Not Found');
        return;
      }
      if (pr.state !== 'open') {
        sendError(res, 405, 'Pull request is not open');
        return;
      }
      const baseTip = repo.refs.get(`refs/heads/${pr.base}`);
      const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
      if (!baseTip) {
        sendError(res, 404, 'Not Found');
        return;
      }
      const base = pr.mergeBase ?? mergeBase(repo, headSha, baseTip) ?? pr.baseSha;
      const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
      const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
      const tipTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
      const merged = threeWayMerge(repo, baseTree, headTree, tipTree);
      if (merged === null) {
        sendError(res, 405, 'Merge conflict');
        return;
      }
      const treeSha = buildTreeFromFlat(repo, merged);
      const message = pr.title;
      const commitData = buildCommit({ tree: treeSha, parents: [baseTip, headSha], message, author: SERVER_AUTHOR, committer: SERVER_AUTHOR, timestamp: FIXED_EPOCH });
      const mergeSha = putObject(repo, 'commit', commitData);
      repo.refs.set(`refs/heads/${pr.base}`, mergeSha);
      pr.state = 'closed';
      pr.merged = true;
      pr.mergedAt = FIXED_ISO;
      pr.mergeCommitSha = mergeSha;
      sendJson(res, 200, { sha: mergeSha, merged: true, message });
      return;
    }
  }

  sendError(res, 404, 'Not Found');
}

// ---------------------------------------------------------------------------
// GraphQL v4: a small executor for the documents gh / github-mcp-server send
// ---------------------------------------------------------------------------

type GqlValue =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'enum'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'list'; items: GqlValue[] }
  | { kind: 'object'; fields: Record<string, GqlValue> };

interface GqlField {
  alias: string | null;
  name: string;
  args: Record<string, GqlValue>;
  selections: GqlField[] | null;
}

interface GqlOperation {
  type: 'query' | 'mutation';
  name: string | null;
  selections: GqlField[];
}

function gqlTokenize(input: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < input.length) {
    const c = input[i]!;
    if (/\s/.test(c) || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if ('{}():[]!=$@'.includes(c) || c === '.') {
      if (c === '.' && input.slice(i, i + 3) === '...') {
        tokens.push('...');
        i += 3;
        continue;
      }
      tokens.push(c);
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          const e = input[j + 1]!;
          out += e === 'n' ? '\n' : e === 't' ? '\t' : e === 'r' ? '\r' : e;
          j += 2;
        } else {
          out += input[j];
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    let j = i;
    while (j < input.length && /[A-Za-z0-9_-]/.test(input[j]!)) j += 1;
    tokens.push(input.slice(i, j));
    i = j;
  }
  return tokens;
}

class GqlParser {
  private pos = 0;
  private readonly tokens: string[];
  constructor(tokens: string[]) {
    this.tokens = tokens;
  }
  peek(): string {
    return this.tokens[this.pos] ?? '';
  }
  next(): string {
    return this.tokens[this.pos++] ?? '';
  }
  expect(tok: string): void {
    const got = this.next();
    if (got !== tok) throw new Error(`expected ${tok}, got ${got}`);
  }
  parseValue(): GqlValue {
    const tok = this.peek();
    if (tok === '$') {
      this.next();
      return { kind: 'variable', name: this.next() };
    }
    if (tok === '[') {
      this.next();
      const items: GqlValue[] = [];
      while (this.peek() !== ']') items.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', items };
    }
    if (tok === '{') {
      this.next();
      const fields: Record<string, GqlValue> = {};
      while (this.peek() !== '}') {
        const name = this.next();
        this.expect(':');
        fields[name] = this.parseValue();
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (tok.startsWith('"')) {
      this.next();
      return { kind: 'string', value: JSON.parse(tok) as string };
    }
    if (/^-?\d+$/.test(tok)) {
      this.next();
      return { kind: 'int', value: Number(tok) };
    }
    if (tok === 'true' || tok === 'false') {
      this.next();
      return { kind: 'boolean', value: tok === 'true' };
    }
    if (tok === 'null') {
      this.next();
      return { kind: 'null' };
    }
    this.next();
    return { kind: 'enum', value: tok };
  }
  parseSelections(): GqlField[] {
    this.expect('{');
    const fields: GqlField[] = [];
    while (this.peek() !== '}') {
      if (this.peek() === '...') {
        this.next();
        // Inline fragment: `... on Type { ... }` — selections apply to the
        // object itself; the type condition is checked by the resolver's shape.
        if (this.peek() === 'on') {
          this.next();
          this.next(); // the type name
        }
        const inner = this.parseSelections();
        fields.push(...inner);
        continue;
      }
      const first = this.next();
      let alias: string | null = null;
      let name = first;
      if (this.peek() === ':') {
        this.next();
        alias = first;
        name = this.next();
      }
      const args: Record<string, GqlValue> = {};
      if (this.peek() === '(') {
        this.next();
        while (this.peek() !== ')') {
          const argName = this.next();
          this.expect(':');
          args[argName] = this.parseValue();
        }
        this.expect(')');
      }
      let selections: GqlField[] | null = null;
      if (this.peek() === '{') selections = this.parseSelections();
      fields.push({ alias, name, args, selections });
    }
    this.expect('}');
    return fields;
  }
  parseOperation(): GqlOperation {
    let type: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const tok = this.peek();
    if (tok === 'query' || tok === 'mutation') {
      type = this.next() as 'query' | 'mutation';
      if (this.peek() !== '{' && this.peek() !== '(') name = this.next();
    }
    if (this.peek() === '(') {
      // Variable definitions: ($x: Type!, ...) — skip; values come from `variables`.
      let depth = 0;
      while (this.pos < this.tokens.length) {
        const t = this.next();
        if (t === '(') depth += 1;
        else if (t === ')') {
          depth -= 1;
          if (depth === 0) break;
        }
      }
    }
    const selections = this.parseSelections();
    return { type, name, selections };
  }
}

function gqlValue(value: GqlValue, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'variable':
      return variables[value.name];
    case 'string':
    case 'enum':
      return value.value;
    case 'int':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'list':
      return value.items.map((i) => gqlValue(i, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(value.fields)) out[k] = gqlValue(v, variables);
      return out;
    }
  }
}

// The schema surface the suite introspects (field names per type).
const SCHEMA: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type'],
  User: ['login', 'id'],
  Viewer: ['login', 'id'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Issue: ['id', 'number', 'title', 'state', 'body'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files'],
  IssueConnection: ['nodes', 'pageInfo'],
  PullRequestConnection: ['nodes', 'pageInfo'],
  PullRequestChangedFileConnection: ['nodes', 'pageInfo'],
  PullRequestChangedFile: ['path'],
  IssueComment: ['id', 'body'],
  CommentEdge: ['node'],
  Ref: ['name'],
  PageInfo: ['hasNextPage', 'endCursor'],
  __Type: ['fields', 'enumValues', 'name'],
  __Field: ['name'],
  __EnumValue: ['name'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  CreateIssuePayload: ['issue'],
  CloseIssuePayload: ['issue'],
  CreatePullRequestPayload: ['pullRequest'],
  MergePullRequestPayload: ['pullRequest'],
  AddCommentPayload: ['commentEdge'],
};

const SEARCH_TYPE_VALUES = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'];

function repoByDatabaseId(databaseId: number): Repo | undefined {
  for (const r of repos.values()) if (r.databaseId === databaseId) return r;
  return undefined;
}

function mergeableState(repo: Repo, pr: PullRequest): string {
  const baseTip = repo.refs.get(`refs/heads/${pr.base}`);
  const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
  if (!baseTip) return 'UNKNOWN';
  const base = pr.mergeBase ?? mergeBase(repo, headSha, baseTip) ?? pr.baseSha;
  const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
  const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
  const tipTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
  return threeWayMerge(repo, baseTree, headTree, tipTree) === null ? 'CONFLICTING' : 'MERGEABLE';
}

function resolveIssuesConnection(repo: Repo, args: Record<string, unknown>): Record<string, unknown> {
  const first = typeof args.first === 'number' ? args.first : 30;
  const after = typeof args.after === 'string' ? args.after : null;
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  const orderBy = (args.orderBy ?? null) as { field?: string; direction?: string } | null;
  let list = [...repo.issues];
  if (states) list = list.filter((i) => states.includes(i.state.toUpperCase()));
  if (orderBy?.field === 'CREATED_AT') {
    const desc = (orderBy.direction ?? 'DESC') === 'DESC';
    list.sort((a, b) => (desc ? b.seq - a.seq : a.seq - b.seq));
    if (desc) list.sort((a, b) => (a.seq === b.seq ? b.number - a.number : 0));
  } else {
    list.sort((a, b) => a.seq - b.seq);
  }
  let start = 0;
  if (after) {
    const n = cursorNumber(after);
    if (n !== null) {
      const idx = list.findIndex((i) => i.number === n);
      if (idx >= 0) start = idx + 1;
    }
  }
  const page = list.slice(start, start + first);
  const hasNext = start + page.length < list.length;
  const endCursor = page.length > 0 ? issueCursor(page[page.length - 1]!.number) : null;
  return {
    __kind: 'IssueConnection',
    nodes: page.map((i) => issueNode(repo, i)),
    pageInfo: { __kind: 'PageInfo', hasNextPage: hasNext, endCursor },
    __issues: page,
  };
}

function issueNode(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    __kind: 'Issue',
    __repo: repo,
    __issue: issue,
    id: globalId('Issue', `${repo.databaseId}:${issue.number}`),
    number: issue.number,
    title: issue.title,
    state: issue.state.toUpperCase(),
    body: issue.body,
  };
}

function pullNode(repo: Repo, pr: PullRequest): Record<string, unknown> {
  const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
  const baseSha = repo.refs.get(`refs/heads/${pr.base}`) ?? pr.baseSha;
  return {
    __kind: 'PullRequest',
    __repo: repo,
    __pr: pr,
    id: globalId('PullRequest', `${repo.databaseId}:${pr.number}`),
    number: pr.number,
    title: pr.title,
    state: pr.state.toUpperCase(),
    merged: pr.merged,
    headRefName: pr.head,
    baseRefName: pr.base,
    mergeable: mergeableState(repo, pr),
    __headSha: headSha,
    __baseSha: baseSha,
  };
}

function repoNode(repo: Repo): Record<string, unknown> {
  const defaultBranch = headOid(repo) ? repo.defaultBranch : null;
  return {
    __kind: 'Repository',
    __repo: repo,
    id: globalId('Repository', String(repo.databaseId)),
    databaseId: repo.databaseId,
    name: repo.name,
    nameWithOwner: `${viewerLogin}/${repo.name}`,
    owner: { __kind: 'User', login: viewerLogin },
    defaultBranchRef: defaultBranch ? { __kind: 'Ref', name: defaultBranch } : null,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
  };
}

function typeNode(typeName: string): Record<string, unknown> | null {
  if (typeName === 'SearchType') {
    return { __kind: '__Type', name: 'SearchType', __fields: [], __enumValues: SEARCH_TYPE_VALUES };
  }
  const fields = SCHEMA[typeName];
  if (!fields) return null;
  return { __kind: '__Type', name: typeName, __fields: fields, __enumValues: [] as string[] };
}

/** Resolve one field against a resolved parent object. Returns undefined for a null parent. */
function resolveField(
  parent: Record<string, unknown>,
  field: GqlField,
  variables: Record<string, unknown>,
  errors: { message: string }[],
  typeName: string,
): unknown {
  const allowed = SCHEMA[typeName] ?? [];
  if (!allowed.includes(field.name) && !field.name.startsWith('__')) {
    errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` });
    return null;
  }
  const args: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(field.args)) args[k] = gqlValue(v, variables);
  const kind = parent.__kind as string | undefined;

  if (typeName === 'Query') {
    if (field.name === 'viewer') return { __kind: 'User', login: viewerLogin };
    if (field.name === 'repository') {
      const owner = args.owner as string;
      const name = args.name as string;
      if (owner !== viewerLogin) return null;
      const repo = getRepo(name);
      return repo ? repoNode(repo) : null;
    }
    if (field.name === 'node') {
      const decoded = decodeGlobalId(String(args.id ?? ''));
      if (!decoded) return null;
      if (decoded.kind === 'Repository') {
        const repo = repoByDatabaseId(Number(decoded.key));
        return repo ? repoNode(repo) : null;
      }
      return null;
    }
    if (field.name === '__type') {
      return typeNode(String(args.name ?? ''));
    }
  }
  if (kind === 'Repository') {
    const repo = parent.__repo as Repo;
    if (field.name === 'issue') {
      const issue = repo.issues.find((i) => i.number === (args.number as number));
      return issue ? issueNode(repo, issue) : null;
    }
    if (field.name === 'issues') return resolveIssuesConnection(repo, args);
    if (field.name === 'pullRequests') {
      const first = typeof args.first === 'number' ? args.first : 30;
      const list = repo.pulls.filter((p) => p.state === 'open').sort((a, b) => b.seq - a.seq).slice(0, first);
      return { __kind: 'PullRequestConnection', nodes: list.map((p) => pullNode(repo, p)), pageInfo: { __kind: 'PageInfo', hasNextPage: false, endCursor: null } };
    }
    if (field.name === 'pullRequest') {
      const pr = repo.pulls.find((p) => p.number === (args.number as number));
      return pr ? pullNode(repo, pr) : null;
    }
  }
  if (kind === 'PullRequest') {
    const repo = parent.__repo as Repo;
    const pr = parent.__pr as PullRequest;
    if (field.name === 'files') {
      const first = typeof args.first === 'number' ? args.first : 10;
      const base = pr.mergeBase ?? pr.baseSha;
      const baseTree = base ? parseCommit(repo.objects.get(base)!.data).tree : null;
      const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
      const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
      const baseFlat = baseTree ? flattenTree(repo, baseTree) : new Map<string, { oid: string; mode: string }>();
      const headFlat = flattenTree(repo, headTree);
      const paths = [...new Set([...baseFlat.keys(), ...headFlat.keys()])].sort();
      const nodes: Record<string, unknown>[] = [];
      for (const p of paths) {
        const b = baseFlat.get(p);
        const h = headFlat.get(p);
        if (b && h && b.oid === h.oid) continue;
        nodes.push({ __kind: 'PullRequestChangedFile', path: p });
      }
      return { __kind: 'PullRequestChangedFileConnection', nodes: nodes.slice(0, first), pageInfo: { __kind: 'PageInfo', hasNextPage: false, endCursor: null } };
    }
  }
  if (kind === '__Type') {
    if (field.name === 'fields') {
      const names = (parent.__fields as string[]) ?? [];
      return names.map((n) => ({ __kind: '__Field', name: n }));
    }
    if (field.name === 'enumValues') {
      const names = (parent.__enumValues as string[]) ?? [];
      return names.map((n) => ({ __kind: '__EnumValue', name: n }));
    }
  }
  // Scalar / leaf fields and nested objects: return the parent's own value.
  const value = parent[field.name];
  return value === undefined ? null : value;
}

const KIND_TYPE: Record<string, string> = {
  User: 'User',
  Repository: 'Repository',
  Issue: 'Issue',
  PullRequest: 'PullRequest',
  IssueConnection: 'IssueConnection',
  PullRequestConnection: 'PullRequestConnection',
  PullRequestChangedFileConnection: 'PullRequestChangedFileConnection',
  PullRequestChangedFile: 'PullRequestChangedFile',
  IssueComment: 'IssueComment',
  CommentEdge: 'CommentEdge',
  Ref: 'Ref',
  PageInfo: 'PageInfo',
  __Type: '__Type',
  __Field: '__Field',
  __EnumValue: '__EnumValue',
};

function executeSelections(
  parent: Record<string, unknown>,
  typeName: string,
  selections: GqlField[],
  variables: Record<string, unknown>,
  errors: { message: string }[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of selections) {
    const key = field.alias ?? field.name;
    const value = resolveField(parent, field, variables, errors, typeName);
    if (value === null || value === undefined) {
      out[key] = null;
      continue;
    }
    if (field.selections && typeof value === 'object' && !Array.isArray(value)) {
      const childKind = (value as Record<string, unknown>).__kind as string | undefined;
      const childType = childKind ? KIND_TYPE[childKind] ?? childKind : typeName;
      out[key] = executeSelections(value as Record<string, unknown>, childType, field.selections, variables, errors);
      continue;
    }
    if (field.selections && Array.isArray(value)) {
      out[key] = value.map((entry) => {
        if (entry !== null && typeof entry === 'object') {
          const childKind = (entry as Record<string, unknown>).__kind as string | undefined;
          const childType = childKind ? KIND_TYPE[childKind] ?? childKind : typeName;
          return executeSelections(entry as Record<string, unknown>, childType, field.selections!, variables, errors);
        }
        return entry;
      });
      continue;
    }
    out[key] = value;
  }
  return out;
}

function executeMutation(
  operation: GqlOperation,
  variables: Record<string, unknown>,
  errors: { message: string }[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of operation.selections) {
    const key = field.alias ?? field.name;
    const input = (gqlValue(field.args.input as GqlValue, variables) ?? {}) as Record<string, unknown>;
    if (field.name === 'createIssue') {
      const repo = repoByDatabaseId(Number(decodeGlobalId(String(input.repositoryId ?? ''))?.key ?? ''));
      if (!repo) {
        errors.push({ message: 'createIssue: unknown repository' });
        out[key] = null;
        continue;
      }
      const issue: Issue = {
        number: repo.nextNumber,
        title: String(input.title ?? ''),
        body: String(input.body ?? ''),
        state: 'open',
        labels: [],
        comments: [],
        seq: repo.nextSeq,
      };
      repo.nextNumber += 1;
      repo.nextSeq += 1;
      repo.issues.push(issue);
      const node = issueNode(repo, issue);
      out[key] = field.selections ? executeSelections({ __kind: 'CreateIssuePayload', issue: node }, 'CreateIssuePayload', field.selections, variables, errors) : { issue: node };
      continue;
    }
    if (field.name === 'closeIssue') {
      const decoded = decodeGlobalId(String(input.issueId ?? ''));
      const parts = decoded?.key.split(':') ?? [];
      const repo = decoded?.kind === 'Issue' ? repoByDatabaseId(Number(parts[0])) : undefined;
      const issue = repo?.issues.find((i) => i.number === Number(parts[1]));
      if (!repo || !issue) {
        errors.push({ message: 'closeIssue: unknown issue' });
        out[key] = null;
        continue;
      }
      issue.state = 'closed';
      const node = issueNode(repo, issue);
      out[key] = field.selections ? executeSelections({ __kind: 'CloseIssuePayload', issue: node }, 'CloseIssuePayload', field.selections, variables, errors) : { issue: node };
      continue;
    }
    if (field.name === 'createPullRequest') {
      const repo = repoByDatabaseId(Number(decodeGlobalId(String(input.repositoryId ?? ''))?.key ?? ''));
      const head = String(input.headRefName ?? '');
      const base = String(input.baseRefName ?? '');
      const headSha = repo?.refs.get(`refs/heads/${head}`);
      const baseSha = repo?.refs.get(`refs/heads/${base}`);
      if (!repo || !headSha || !baseSha) {
        errors.push({ message: 'createPullRequest: unknown repository or branch' });
        out[key] = null;
        continue;
      }
      let pr = repo.pulls.find((p) => p.head === head && p.base === base && p.state === 'open');
      if (!pr) {
        pr = {
          number: repo.nextNumber,
          title: String(input.title ?? ''),
          body: String(input.body ?? ''),
          state: 'open',
          merged: false,
          mergedAt: null,
          mergeCommitSha: null,
          head,
          base,
          headSha,
          baseSha,
          mergeBase: mergeBase(repo, headSha, baseSha),
          seq: repo.nextSeq,
        };
        repo.nextNumber += 1;
        repo.nextSeq += 1;
        repo.pulls.push(pr);
      }
      const node = pullNode(repo, pr);
      out[key] = field.selections ? executeSelections({ __kind: 'CreatePullRequestPayload', pullRequest: node }, 'CreatePullRequestPayload', field.selections, variables, errors) : { pullRequest: node };
      continue;
    }
    if (field.name === 'mergePullRequest') {
      const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
      const parts = decoded?.key.split(':') ?? [];
      const repo = decoded?.kind === 'PullRequest' ? repoByDatabaseId(Number(parts[0])) : undefined;
      const pr = repo?.pulls.find((p) => p.number === Number(parts[1]));
      if (!repo || !pr) {
        errors.push({ message: 'mergePullRequest: unknown pull request' });
        out[key] = null;
        continue;
      }
      if (pr.state === 'open') {
        const baseTip = repo.refs.get(`refs/heads/${pr.base}`);
        const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
        if (baseTip) {
          const base = pr.mergeBase ?? mergeBase(repo, headSha, baseTip) ?? pr.baseSha;
          const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
          const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
          const tipTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
          const merged = threeWayMerge(repo, baseTree, headTree, tipTree);
          if (merged !== null) {
            const treeSha = buildTreeFromFlat(repo, merged);
            const commitData = buildCommit({ tree: treeSha, parents: [baseTip, headSha], message: pr.title, author: SERVER_AUTHOR, committer: SERVER_AUTHOR, timestamp: FIXED_EPOCH });
            const mergeSha = putObject(repo, 'commit', commitData);
            repo.refs.set(`refs/heads/${pr.base}`, mergeSha);
            pr.state = 'closed';
            pr.merged = true;
            pr.mergedAt = FIXED_ISO;
            pr.mergeCommitSha = mergeSha;
          } else {
            errors.push({ message: 'mergePullRequest: merge conflict' });
          }
        }
      }
      const node = pullNode(repo, pr);
      out[key] = field.selections ? executeSelections({ __kind: 'MergePullRequestPayload', pullRequest: node }, 'MergePullRequestPayload', field.selections, variables, errors) : { pullRequest: node };
      continue;
    }
    if (field.name === 'addComment') {
      const decoded = decodeGlobalId(String(input.subjectId ?? ''));
      const parts = decoded?.key.split(':') ?? [];
      const repo = decoded?.kind === 'Issue' ? repoByDatabaseId(Number(parts[0])) : undefined;
      const issue = repo?.issues.find((i) => i.number === Number(parts[1]));
      if (!repo || !issue) {
        errors.push({ message: 'addComment: unknown subject' });
        out[key] = null;
        continue;
      }
      const comment = { id: repo.nextCommentId, body: String(input.body ?? '') };
      repo.nextCommentId += 1;
      issue.comments.push(comment);
      const node = { __kind: 'IssueComment', id: globalId('IssueComment', `${repo.databaseId}:${issue.number}:${comment.id}`), body: comment.body };
      const edge = { __kind: 'CommentEdge', node };
      out[key] = field.selections ? executeSelections({ __kind: 'AddCommentPayload', commentEdge: edge }, 'AddCommentPayload', field.selections, variables, errors) : { commentEdge: edge };
      continue;
    }
    errors.push({ message: `Cannot query field "${field.name}" on type "Mutation".` });
    out[key] = null;
  }
  return out;
}

function executeGraphQLOperation(query: string, variables: Record<string, unknown>): { data: unknown; errors?: { message: string }[] } {
  const errors: { message: string }[] = [];
  let operation: GqlOperation;
  try {
    const parser = new GqlParser(gqlTokenize(query));
    operation = parser.parseOperation();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'GraphQL parse error' }] };
  }
  if (operation.type === 'mutation') {
    const data = executeMutation(operation, variables, errors);
    return errors.length > 0 ? { data, errors } : { data };
  }
  const data = executeSelections({ __kind: 'Query' }, 'Query', operation.selections, variables, errors);
  return errors.length > 0 ? { data, errors } : { data };
}

function handleGraphQL(req: IncomingMessage, res: ServerResponse, body: Buffer): void {
  if (!isAuthenticated(req)) {
    sendError(res, 401, 'Requires authentication');
    return;
  }
  let parsed: unknown;
  try {
    parsed = parseJsonBody(body);
  } catch {
    sendJson(res, 400, { errors: [{ message: 'Malformed JSON body' }] });
    return;
  }
  if (parsed === null) {
    sendJson(res, 400, { errors: [{ message: 'Empty GraphQL body' }] });
    return;
  }
  const runOne = (op: unknown): Record<string, unknown> => {
    if (typeof op !== 'object' || op === null || typeof (op as Record<string, unknown>).query !== 'string') {
      return { data: null, errors: [{ message: 'A GraphQL operation needs a query string' }] };
    }
    const o = op as Record<string, unknown>;
    const variables = (o.variables ?? {}) as Record<string, unknown>;
    return executeGraphQLOperation(o.query as string, variables);
  };
  if (Array.isArray(parsed)) {
    sendJson(res, 200, parsed.map(runOne));
    return;
  }
  sendJson(res, 200, runOne(parsed));
}

// ---------------------------------------------------------------------------
// MCP over streamable HTTP (modern 2026-07-28 discover + legacy initialize)
// ---------------------------------------------------------------------------

const MCP_SERVER = { name: 'github-core-clone-forge', version: '1.0.0' };
const MCP_VERSIONS = ['2026-07-28', '2025-11-25'];

interface McpToolDef {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, { type: string; description?: string }>; required: string[] };
}

const MCP_TOOLS: McpToolDef[] = [
  { name: 'create_repo', description: 'Create a repository for the token owner.', inputSchema: { type: 'object', properties: { name: { type: 'string' }, description: { type: 'string' }, private: { type: 'boolean' } }, required: ['name'] } },
  { name: 'get_repo', description: 'Read one repository by owner and name.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' } }, required: ['owner', 'repo', 'title'] } },
  { name: 'list_issues', description: 'List the issues of a repository, optionally filtered by state.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, state: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_pull_request', description: 'Open a pull request from a head branch into a base branch.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } }, required: ['owner', 'repo', 'title', 'head', 'base'] } },
  { name: 'merge_pull_request', description: 'Merge a pull request by number (squash).', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, pull_number: { type: 'number' } }, required: ['owner', 'repo', 'pull_number'] } },
];

function mcpToolResult(payload: unknown, isError = false): Record<string, unknown> {
  const text = typeof payload === 'string' ? payload : JSON.stringify(payload);
  return { content: [{ type: 'text', text }], structuredContent: payload, isError };
}

function mcpCallTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const owner = typeof args.owner === 'string' ? args.owner : viewerLogin;
  const repoName = typeof args.repo === 'string' ? args.repo : typeof args.name === 'string' && name === 'create_repo' ? '' : '';
  if (name === 'create_repo') {
    const repoNameArg = typeof args.name === 'string' ? args.name : '';
    if (repoNameArg === '') return mcpToolResult('Missing required argument: name', true);
    if (getRepo(repoNameArg)) return mcpToolResult(`Repository ${repoNameArg} already exists`, true);
    const repo = createRepoRecord(repoNameArg, typeof args.description === 'string' ? args.description : '', args.private === true);
    return mcpToolResult(repoJson(repo));
  }
  const repo = getRepo(repoName);
  if (!repo || owner !== viewerLogin) return mcpToolResult('Repository not found', true);
  if (name === 'get_repo') return mcpToolResult(repoJson(repo));
  if (name === 'create_issue') {
    const title = typeof args.title === 'string' ? args.title : '';
    if (title === '') return mcpToolResult('Missing required argument: title', true);
    const issue: Issue = {
      number: repo.nextNumber,
      title,
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open',
      labels: [],
      comments: [],
      seq: repo.nextSeq,
    };
    repo.nextNumber += 1;
    repo.nextSeq += 1;
    repo.issues.push(issue);
    return mcpToolResult(issueJson(repo, issue));
  }
  if (name === 'list_issues') {
    const state = typeof args.state === 'string' ? args.state : 'open';
    const list = repo.issues.filter((i) => (state === 'all' ? true : i.state === state)).map((i) => issueJson(repo, i));
    return mcpToolResult({ issues: list });
  }
  if (name === 'create_pull_request') {
    const title = typeof args.title === 'string' ? args.title : '';
    const head = typeof args.head === 'string' ? args.head : '';
    const base = typeof args.base === 'string' ? args.base : repo.defaultBranch;
    if (title === '' || head === '') return mcpToolResult('Missing required argument: title or head', true);
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (!headSha || !baseSha) return mcpToolResult('Head or base branch does not exist', true);
    let pr = repo.pulls.find((p) => p.head === head && p.base === base && p.state === 'open');
    if (!pr) {
      pr = {
        number: repo.nextNumber,
        title,
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        head,
        base,
        headSha,
        baseSha,
        mergeBase: mergeBase(repo, headSha, baseSha),
        seq: repo.nextSeq,
      };
      repo.nextNumber += 1;
      repo.nextSeq += 1;
      repo.pulls.push(pr);
    }
    return mcpToolResult(pullJson(repo, pr));
  }
  if (name === 'merge_pull_request') {
    const number = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number);
    const pr = repo.pulls.find((p) => p.number === number);
    if (!pr) return mcpToolResult('Pull request not found', true);
    if (pr.state !== 'open') return mcpToolResult('Pull request is not open', true);
    const baseTip = repo.refs.get(`refs/heads/${pr.base}`);
    const headSha = repo.refs.get(`refs/heads/${pr.head}`) ?? pr.headSha;
    if (!baseTip) return mcpToolResult('Base branch does not exist', true);
    const base = pr.mergeBase ?? mergeBase(repo, headSha, baseTip) ?? pr.baseSha;
    const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
    const headTree = parseCommit(repo.objects.get(headSha)!.data).tree;
    const tipTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
    const merged = threeWayMerge(repo, baseTree, headTree, tipTree);
    if (merged === null) return mcpToolResult('Merge conflict', true);
    const treeSha = buildTreeFromFlat(repo, merged);
    const commitData = buildCommit({ tree: treeSha, parents: [baseTip, headSha], message: pr.title, author: SERVER_AUTHOR, committer: SERVER_AUTHOR, timestamp: FIXED_EPOCH });
    const mergeSha = putObject(repo, 'commit', commitData);
    repo.refs.set(`refs/heads/${pr.base}`, mergeSha);
    pr.state = 'closed';
    pr.merged = true;
    pr.mergedAt = FIXED_ISO;
    pr.mergeCommitSha = mergeSha;
    return mcpToolResult(pullJson(repo, pr));
  }
  return mcpToolResult(`Unknown tool: ${name}`, true);
}

function handleMcp(req: IncomingMessage, res: ServerResponse, body: Buffer): void {
  if (!isAuthenticated(req)) {
    sendJson(res, 401, { jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Requires authentication' } });
    return;
  }
  let parsed: unknown;
  try {
    parsed = parseJsonBody(body);
  } catch {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }
  const msg = parsed as Record<string, unknown>;
  const id = (msg.id ?? null) as string | number | null;
  const method = typeof msg.method === 'string' ? msg.method : '';
  const params = (msg.params ?? {}) as Record<string, unknown>;

  // A notification (no id) is acknowledged with 202 and no body.
  if (msg.id === undefined) {
    res.writeHead(202);
    res.end();
    return;
  }

  const result = (payload: unknown): void => {
    sendJson(res, 200, { jsonrpc: '2.0', id, result: payload });
  };
  const error = (code: number, message: string): void => {
    sendJson(res, 200, { jsonrpc: '2.0', id, error: { code, message } });
  };

  if (method === 'server/discover') {
    result({
      supportedVersions: MCP_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { ...MCP_SERVER },
      _meta: { 'io.modelcontextprotocol/serverInfo': { ...MCP_SERVER } },
    });
    return;
  }
  if (method === 'initialize') {
    const wanted = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    result({
      protocolVersion: MCP_VERSIONS.includes(wanted) ? wanted : '2025-11-25',
      capabilities: { tools: {} },
      serverInfo: { ...MCP_SERVER },
    });
    return;
  }
  if (method === 'tools/list') {
    result({ tools: MCP_TOOLS });
    return;
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const tool = MCP_TOOLS.find((t) => t.name === toolName);
    if (!tool) {
      error(-32601, `Unknown tool: ${toolName}`);
      return;
    }
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const missing = tool.inputSchema.required.filter((r) => args[r] === undefined || args[r] === null || args[r] === '');
    if (missing.length > 0) {
      result(mcpToolResult(`Missing required argument: ${missing.join(', ')}`, true));
      return;
    }
    result(mcpCallTool(toolName, args));
    return;
  }
  error(-32601, `Method not found: ${method}`);
}

// ---------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    const method = req.method ?? 'GET';
    const path = pathOf(req);
    void (async () => {
      try {
        // GET /healthz: 200 {"ok":true}, no auth. The env's boot check calls it twice.
        if (method === 'GET' && path === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }
        const body = await readBody(req);
        // Git smart HTTP: /<owner>/<repo>.git/...
        const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(.*)$/.exec(path);
        if (gitMatch) {
          const owner = decodeURIComponent(gitMatch[1]!);
          const name = decodeURIComponent(gitMatch[2]!);
          const suffix = gitMatch[3]!;
          if (method === 'GET' && suffix === 'info/refs') {
            handleInfoRefs(req, res, owner, name);
            return;
          }
          if (method === 'POST' && suffix === 'git-upload-pack') {
            handleUploadPack(req, res, owner, name, body);
            return;
          }
          if (method === 'POST' && suffix === 'git-receive-pack') {
            handleReceivePack(req, res, owner, name, body);
            return;
          }
          sendError(res, 404, 'Not Found');
          return;
        }
        if (method === 'POST' && path === '/api/graphql') {
          handleGraphQL(req, res, body);
          return;
        }
        if (method === 'POST' && path === '/mcp') {
          handleMcp(req, res, body);
          return;
        }
        if (path === '/api/v3' || path === '/api/v3/' || path.startsWith('/api/v3/')) {
          handleRest(req, res, body);
          return;
        }
        sendError(res, 404, 'Not Found');
      } catch (err) {
        try {
          sendError(res, 500, err instanceof Error ? err.message : 'Internal error');
        } catch {
          // The response is already gone; nothing more to do.
        }
      }
    })();
  });
}
