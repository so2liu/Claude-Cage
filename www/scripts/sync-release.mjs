// Public downloads and metadata stay anonymous, including the manifest fallback.
import { createHash } from 'node:crypto';
import { createWriteStream } from 'node:fs';
import { mkdir, writeFile, rm } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { Readable, Transform } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { parseArgs } from 'node:util';

const repository = 'Lee-xeo/Claude-Cage';
const repoURL = `https://github.com/${repository}`;
const apiURL = `https://api.github.com/repos/${repository}/releases`;
const sourceArchive = name => /^mihomo-v[\d.]+-source\.tar\.gz$/.test(name) ||
  /^cage-macos-guest-\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?-source\.tar\.gz$/.test(name) ||
  /^qemu-[0-9a-f]{40}-source\.tar\.gz$/.test(name) ||
  /^qemu-keycodemapdb-[0-9a-f]{40}-source\.tar\.gz$/.test(name) ||
  /^qemu-\d+\.\d+\.\d+-cage-changes-source\.tar\.gz$/.test(name) ||
  /^qemu-\d+\.\d+\.\d+-windows-full-source\.(?:tar\.)?(?:gz|xz|bz2|zst|zip|7z|tar)$/.test(name);

export async function request(url, timeout = 180_000, { fetchImpl = fetch } = {}) {
  const target = new URL(url);
  const metadata = target.origin === 'https://api.github.com' &&
    (target.pathname === `/repos/${repository}/releases` || target.pathname.startsWith(`/repos/${repository}/releases/`));
  const options = {
    headers: { 'User-Agent': 'Cage-Release-Verification', Accept: metadata ? 'application/vnd.github+json' : 'application/octet-stream' },
    signal: AbortSignal.timeout(timeout),
  };
  let response = await fetchImpl(url, options);
  if (metadata && response.status === 403 && response.headers.get('x-ratelimit-remaining') === '0') {
    const tag = target.pathname.match(/^\/repos\/Lee-xeo\/Claude-Cage\/releases\/tags\/(v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/)?.[1];
    if (tag) {
      await response.body?.cancel();
      console.warn('Anonymous GitHub API is rate limited; reading the requested version from its public checksum manifest.');
      return new Response(JSON.stringify(await publicManifestRelease(tag, fetchImpl)));
    }
  }
  if (!response.ok) throw new Error(`Public download failed: HTTP ${response.status} (${new URL(url).pathname})`);
  return response;
}

export async function publicManifestRelease(tag, fetchImpl = fetch) {
  if (!/^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(tag)) throw new Error('Invalid public release tag');
  const base = `${repoURL}/releases/download/${tag}/`;
  const manifest = await request(`${base}SHA256SUMS`, 180_000, { fetchImpl });
  const text = await manifest.text();
  if (!text.length || Buffer.byteLength(text) > 65536) throw new Error('Invalid public checksum manifest size');
  const names = new Map();
  for (const line of text.split(/\r?\n/).filter(Boolean)) {
    const match = /^([a-f0-9]{64}) [ *]([A-Za-z0-9][A-Za-z0-9._-]*)$/i.exec(line);
    if (!match || names.has(match[2])) throw new Error('Invalid or duplicate public checksum entry');
    names.set(match[2], match[1].toLowerCase());
  }
  const version = tag.slice(1);
  const allowed = name => [
    `Cage-${version}-macos-arm64.dmg`, `Cage-${version}-windows-amd64-setup.exe`,
    `Cage-${version}-windows-amd64-setup-unsigned.exe`, `Cage-${version}-linux-amd64.deb`,
    `Cage-${version}-android-arm64.apk`,
  ].includes(name) || sourceArchive(name);
  if ([...names.keys()].some(name => !allowed(name))) throw new Error('Unexpected public release asset');
  const assets = [{ name: 'SHA256SUMS', state: 'uploaded', size: Buffer.byteLength(text), browser_download_url: `${base}SHA256SUMS` }];
  for (const [name, digest] of names) {
    const response = await fetchImpl(`${base}${name}`, { method: 'HEAD', headers: { 'User-Agent': 'Cage-Release-Verification' }, signal: AbortSignal.timeout(180_000) });
    const size = Number(response.headers.get('content-length'));
    const maximum = (sourceArchive(name) ? 2048 : 512) * 1024 * 1024;
    if (!response.ok || !Number.isSafeInteger(size) || size <= 0 || size > maximum) throw new Error(`Missing or incomplete public asset: ${name}`);
    assets.push({ name, state: 'uploaded', size, digest: `sha256:${digest}`, browser_download_url: `${base}${name}` });
  }
  const release = { tag_name: tag, draft: false, prerelease: version.includes('-'), assets };
  releaseAssets(release);
  return release;
}

export function releaseAssets(release) {
  if (release.draft || !/^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(release.tag_name)) {
    throw new Error('Expected a published Cage version tag');
  }
  const version = release.tag_name.slice(1);
  const names = [`Cage-${version}-macos-arm64.dmg`, 'SHA256SUMS'];
  const assets = names.map(name => release.assets.find(asset => asset.name === name));
  const source = release.assets.find(asset => /^mihomo-v[\d.]+-source\.tar\.gz$/.test(asset.name));
  assets.push(source);
  for (const asset of assets) {
    if (!asset || asset.state !== 'uploaded' || !Number.isSafeInteger(asset.size) || asset.size <= 0 || asset.size > 512 * 1024 * 1024) {
      throw new Error(`Release ${release.tag_name} is missing a complete DMG, checksum file, or Mihomo source`);
    }
    if (asset.browser_download_url !== `${repoURL}/releases/download/${release.tag_name}/${asset.name}`) {
      throw new Error('Release asset is outside the expected repository and tag');
    }
  }
  if (assets[1].size > 64 * 1024) throw new Error('Checksum file is too large');
  const windows = optionalAsset(release, `Cage-${version}-windows-amd64-setup.exe`);
  const linux = optionalAsset(release, `Cage-${version}-linux-amd64.deb`);
  const android = optionalAsset(release, `Cage-${version}-android-arm64.apk`);
  const guestSource = optionalAsset(release, `cage-macos-guest-${version}-source.tar.gz`);
  return { version, archive: assets[0], checksums: assets[1], source, windows, linux, android, guestSource };
}

function optionalAsset(release, name) {
  const matches = release.assets.filter(asset => asset.name === name);
  const asset = matches.length === 1 ? matches[0] : undefined;
  return asset && asset.state === 'uploaded' && Number.isSafeInteger(asset.size) &&
    asset.size > 0 && asset.size <= 512 * 1024 * 1024 &&
    asset.browser_download_url === `${repoURL}/releases/download/${release.tag_name}/${name}`
    ? asset : undefined;
}

export function checksumFor(text, asset) {
  const entries = text.split(/\r?\n/).map(line => line.trim().split(/\s+/))
    .filter(fields => fields.length === 2 && fields[1].replace(/^\*/, '') === asset.name);
  if (entries.length !== 1 || !/^[a-f0-9]{64}$/i.test(entries[0][0])) throw new Error(`Missing or duplicate SHA-256 for ${asset.name}`);
  const digest = entries[0][0].toLowerCase();
  if (asset.digest && asset.digest.toLowerCase() !== `sha256:${digest}`) throw new Error(`GitHub digest disagrees with SHA256SUMS for ${asset.name}`);
  return digest;
}

export function releaseMetadata(release, text) {
  const { version, archive, source, windows, linux, android } = releaseAssets(release);
  checksumFor(text, archive);
  checksumFor(text, source);
  const metadata = { version, releaseURL: `${repoURL}/releases/tag/v${version}`, downloadURL: archive.browser_download_url };
  for (const [key, asset] of [['windowsDownloadURL', windows], ['linuxDownloadURL', linux], ['androidDownloadURL', android]]) {
    if (!asset) continue;
    try {
      checksumFor(text, asset);
      metadata[key] = asset.browser_download_url;
    } catch {
      // A partial platform upload must not hide other verified downloads.
    }
  }
  if (!metadata.windowsDownloadURL && release.prerelease) {
    const unsigned = optionalAsset(release, `Cage-${version}-windows-amd64-setup-unsigned.exe`);
    const qemu = release.assets.find(asset => /^qemu-[0-9a-f]{40}-source\.tar\.gz$/.test(asset.name));
    if (unsigned && qemu && optionalAsset(release, qemu.name)) {
      try {
        checksumFor(text, unsigned);
        checksumFor(text, qemu);
        metadata.windowsPreview = { version, releaseURL: metadata.releaseURL, downloadURL: unsigned.browser_download_url };
      } catch {
        // An incomplete unsigned upload must not hide other platform downloads.
      }
    }
  }
  return metadata;
}

async function checksumText(release) {
  const asset = optionalAsset(release, 'SHA256SUMS');
  if (!asset || asset.size > 64 * 1024) throw new Error('Missing complete checksum file');
  const text = await (await request(asset.browser_download_url)).text();
  if (Buffer.byteLength(text) !== asset.size) throw new Error('Checksum file size differs from GitHub metadata');
  return text;
}

async function download(asset, digest, directory) {
  await mkdir(directory, { recursive: true });
  const path = join(directory, asset.name);
  const response = await request(asset.browser_download_url, 15 * 60 * 1000);
  const hash = createHash('sha256');
  let size = 0;
  try {
    await pipeline(Readable.fromWeb(response.body), new Transform({
      transform(chunk, encoding, callback) {
        size += chunk.length;
        if (size > asset.size) return callback(new Error('Release asset exceeds its declared size'));
        hash.update(chunk);
        callback(null, chunk);
      },
    }), createWriteStream(`${path}.part`, { flags: 'wx' }));
    if (size !== asset.size || hash.digest('hex') !== digest) throw new Error(`Downloaded bytes do not match SHA256SUMS: ${asset.name}`);
    const { rename } = await import('node:fs/promises');
    await rename(`${path}.part`, path);
  } finally {
    await rm(`${path}.part`, { force: true });
  }
  console.log(`Verified public download: ${asset.name} (${size} bytes)`);
}

async function main() {
  const { values } = parseArgs({ options: {
    tag: { type: 'string' }, output: { type: 'string', default: 'www/src/data/release.json' },
    'archive-directory': { type: 'string' },
  } });
  let release;
  let releases;
  if (values.tag) {
    release = await (await request(`${apiURL}/tags/${encodeURIComponent(values.tag)}`)).json();
  } else {
    releases = await (await request(`${apiURL}?per_page=100`)).json();
    // GitHub orders by creation time. Show the first complete public Cage release.
    release = releases.find(item => {
      try { releaseAssets(item); return true; } catch { return false; }
    });
    if (!release) throw new Error('No complete public Cage release is available');
  }
  const { version, archive, source, guestSource } = releaseAssets(release);
  const text = await checksumText(release);
  const digest = checksumFor(text, archive);
  const sourceDigest = checksumFor(text, source);
  const metadata = releaseMetadata(release, text);
  if (values['archive-directory']) {
    await download(archive, digest, values['archive-directory']);
    await download(source, sourceDigest, values['archive-directory']);
    if (guestSource) await download(guestSource, checksumFor(text, guestSource), values['archive-directory']);
    await writeFile(join(values['archive-directory'], 'SHA256SUMS'), text);
    for (const url of [metadata.windowsDownloadURL || metadata.windowsPreview?.downloadURL, metadata.linuxDownloadURL, metadata.androidDownloadURL].filter(Boolean)) {
      const asset = release.assets.find(asset => asset.browser_download_url === url);
      await download(asset, checksumFor(text, asset), values['archive-directory']);
    }
  }
  const output = resolve(values.output);
  await mkdir(dirname(output), { recursive: true });
  await writeFile(output, `${JSON.stringify(metadata, null, 2)}\n`);
  console.log(`Website download: Cage ${version}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(error => { console.error(error.message); process.exitCode = 1; });
}
