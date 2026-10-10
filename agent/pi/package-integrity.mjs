// Offline validation uses only Node builtins, never the packages being checked.
import { readFileSync, lstatSync, readdirSync, existsSync } from 'node:fs';
import { join, posix } from 'node:path';
import { createHash } from 'node:crypto';
import { gunzipSync } from 'node:zlib';

export function archiveName(integrity) {
  if (typeof integrity !== 'string' || !/^sha512-[A-Za-z0-9+/]+={0,2}$/.test(integrity)) throw new Error('unsupported package integrity');
  const bytes = Buffer.from(integrity.slice(7), 'base64');
  if (bytes.length !== 64 || bytes.toString('base64') !== integrity.slice(7)) throw new Error('invalid package integrity');
  return bytes.toString('hex') + '.tgz';
}

export function verifyArchive(bytes, integrity) {
  if (bytes.length > 32 << 20 || 'sha512-' + createHash('sha512').update(bytes).digest('base64') !== integrity) throw new Error('package archive mismatch');
}

function paxValues(bytes) {
  const values = {};
  for (let offset = 0; offset < bytes.length;) {
    const space = bytes.indexOf(32, offset);
    if (space < 0) throw new Error('invalid package PAX');
    const size = Number(bytes.subarray(offset, space).toString('ascii'));
    if (!Number.isSafeInteger(size) || size <= space - offset + 2 || offset + size > bytes.length || bytes[offset + size - 1] !== 10) throw new Error('invalid package PAX size');
    const record = bytes.subarray(space + 1, offset + size - 1).toString('utf8');
    const equals = record.indexOf('=');
    if (equals < 1) throw new Error('invalid package PAX value');
    values[record.slice(0, equals)] = record.slice(equals + 1);
    offset += size;
  }
  return values;
}

function inventory(compressed) {
  const bytes = gunzipSync(compressed, { maxOutputLength: 64 << 20 });
  const files = new Map();
  let pax = {}, longName;
  let root;
  const string = (offset, length) => bytes.subarray(offset, offset + length).toString('utf8').replace(/\0.*$/s, '');
  for (let offset = 0; offset + 512 <= bytes.length;) {
    if (bytes.subarray(offset, offset + 512).every(b => b === 0)) {
      if (bytes.length - offset < 1024 || !bytes.subarray(offset).every(b => b === 0) || !files.has('package.json')) throw new Error('incomplete package archive');
      return files;
    }
    const type = String.fromCharCode(bytes[offset + 156]);
    const name = longName || pax.path || [string(offset + 345, 155), string(offset, 100)].filter(Boolean).join('/');
    const headerSize = string(offset + 124, 12).trim();
    if (!/^[0-7]+$/.test(headerSize)) throw new Error('invalid package file size');
    const size = pax.size === undefined ? parseInt(headerSize, 8) : Number(pax.size);
    if (!Number.isSafeInteger(size) || size < 0 || offset + 512 + size > bytes.length) throw new Error('truncated package archive');
    const content = bytes.subarray(offset + 512, offset + 512 + size);
    offset += 512 + Math.ceil(size / 512) * 512;
    if (type === 'x') { pax = paxValues(content); continue; }
    if (type === 'L') { longName = content.toString('utf8').replace(/\0.*$/s, ''); continue; }
    pax = {}; longName = undefined;
    if (type === '5') continue;
    const canonical = posix.normalize(name);
    const slash = canonical.indexOf('/');
    const prefix = canonical.slice(0, slash);
    if ((type !== '0' && type !== '\0') || slash < 1 || name.includes('\\') || name.split('/').includes('..') || root !== undefined && root !== prefix) throw new Error(`unsupported package entry: ${type} ${name}`);
    root = prefix;
    const relative = canonical.slice(slash + 1);
    const digest = createHash('sha256').update(content).digest('hex');
    if (!relative || files.has(relative) && files.get(relative) !== digest) throw new Error(`conflicting package entry: ${relative}`);
    files.set(relative, digest);
  }
  throw new Error('missing package archive termination');
}

function verifyDirectory(directory, files) {
  const seen = new Set();
  function walk(relative = '') {
    for (const entry of readdirSync(join(directory, relative), { withFileTypes: true })) {
      const name = posix.join(relative, entry.name);
      if (entry.isSymbolicLink()) throw new Error('package symlink is not qualified');
      // npm's nested dependency namespace is validated independently below.
      if (name === 'node_modules' && entry.isDirectory()) continue;
      if (entry.isDirectory()) walk(name);
      else if (entry.isFile()) {
        if (!files.has(name) || createHash('sha256').update(readFileSync(join(directory, name))).digest('hex') !== files.get(name)) throw new Error('installed package differs from locked inventory');
        seen.add(name);
      } else throw new Error('unsupported installed package entry');
    }
  }
  walk();
  if (seen.size !== files.size) throw new Error('installed package is incomplete');
}

export function verifyPackages(root, archives) {
  const lock = JSON.parse(readFileSync(join(root, 'package-lock.json'), 'utf8'));
  const expected = lock.packages;
  const installed = new Set();
  function modules(relative) {
    const directory = join(root, relative);
    if (!lstatSync(directory).isDirectory()) throw new Error('unsafe dependency namespace');
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      if (relative.endsWith('node_modules') && (entry.name === '.bin' || entry.name === '.package-lock.json')) continue;
      const name = posix.join(relative, entry.name);
      if (!entry.isDirectory()) throw new Error('unsafe installed package');
      if (entry.name.startsWith('@') && relative.endsWith('node_modules')) { modules(name); continue; }
      const binding = expected[name];
      if (!binding) throw new Error('unqualified dependency');
      const path = join(archives, archiveName(binding.integrity));
      if (!lstatSync(path).isFile()) throw new Error('unsafe package archive');
      const bytes = readFileSync(path);
      verifyArchive(bytes, binding.integrity);
      verifyDirectory(join(root, name), inventory(bytes));
      installed.add(name);
      const nested = posix.join(name, 'node_modules');
      if (existsSync(join(root, nested))) modules(nested);
    }
  }
  modules('node_modules');
  for (const [name, binding] of Object.entries(expected)) {
    if (name && !binding.optional && !installed.has(name)) throw new Error('required dependency missing');
  }
}
