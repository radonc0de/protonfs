// protonfs-bridge: speaks newline-delimited JSON on stdin/stdout and performs
// Proton Drive operations with the official SDK. File contents never cross the
// pipe: downloads are written straight into the cache file the caller names,
// uploads are read straight from a local file.
//
// Request:   {"id": 1, "op": "list", "args": {...}}
// Response:  {"id": 1, "ok": true, "result": ...}
//            {"id": 1, "ok": false, "code": "ENOENT", "error": "message"}
// Progress:  {"id": 1, "event": "progress", "bytes": 12345}   (download only)
import * as fs from 'node:fs';
import { createInterface } from 'node:readline';
import { Readable } from 'node:stream';
import { AbortError, NodeWithSameNameExistsValidationError, ValidationError, ConnectionError, RateLimitedError } from '@protontech/drive-sdk';
import { createClient } from './proton.ts';
import { APP_VERSION } from './session.ts';

type Args = Record<string, any>;

const { client, account } = await createClient();
const inflight = new Map<number, AbortController>();

function send(msg: object) {
    process.stdout.write(JSON.stringify(msg) + '\n');
}

class OpError extends Error {
    constructor(public code: string, msg: string) { super(msg); }
}

function errCode(e: any): string {
    if (e instanceof OpError) return e.code;
    if (e instanceof AbortError) return 'EINTR';
    if (e instanceof NodeWithSameNameExistsValidationError) return 'EEXIST';
    if (e instanceof RateLimitedError) return 'EAGAIN';
    if (e instanceof ConnectionError) return 'EIO';
    if (e instanceof ValidationError) return 'EINVAL';
    const m = String(e?.message ?? '').toLowerCase();
    if (m.includes('not found') || m.includes('does not exist')) return 'ENOENT';
    if (m.includes('permission') || m.includes('not allowed')) return 'EACCES';
    return 'EIO';
}

function nodeJSON(n: any, nameOverride?: string) {
    const rev = n.activeRevision;
    const name = nameOverride ?? (n.name?.ok ? n.name.value : n.name?.error?.name || n.uid);
    return {
        uid: n.uid,
        name,
        type: n.type, // file | folder | album | photo
        size: n.type === 'file' ? (rev?.claimedSize ?? rev?.storageSize ?? 0) : 0,
        mtime: +(rev?.claimedModificationTime ?? n.folder?.claimedModificationTime ?? n.modificationTime ?? 0),
        ctime: +(n.creationTime ?? 0),
        revUid: rev?.uid ?? '',
    };
}

async function results(gen: AsyncGenerator<any>) {
    for await (const r of gen) if (!r.ok) throw r.error;
}

const MIME: Record<string, string> = {
    txt: 'text/plain', md: 'text/markdown', html: 'text/html', json: 'application/json', pdf: 'application/pdf',
    jpg: 'image/jpeg', jpeg: 'image/jpeg', png: 'image/png', gif: 'image/gif', webp: 'image/webp', heic: 'image/heic',
    mp4: 'video/mp4', mov: 'video/quicktime', mkv: 'video/x-matroska', mp3: 'audio/mpeg', flac: 'audio/flac',
    zip: 'application/zip', gz: 'application/gzip', tar: 'application/x-tar', iso: 'application/x-iso9660-image',
    csv: 'text/csv', docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
};
const mimeOf = (name: string) => MIME[name.split('.').pop()!.toLowerCase()] ?? 'application/octet-stream';

const ops: Record<string, (a: Args, id: number, signal: AbortSignal) => Promise<unknown>> = {
    async hello() {
        const root = await client.getMyFilesRootFolder();
        return { appVersion: APP_VERSION, myFilesUid: root.uid };
    },

    async list(a) {
        const out = [];
        for await (const n of client.iterateFolderChildren(a.uid)) out.push(nodeJSON(n));
        return out;
    },

    async devices() {
        const out = [];
        for await (const d of client.iterateDevices()) {
            const root = await client.getNode(d.rootFolderUid);
            out.push(nodeJSON(root, d.name.ok ? d.name.value : d.uid));
        }
        return out;
    },

    async sharedWithMe() {
        const out = [];
        for await (const n of client.iterateSharedNodesWithMe()) out.push(nodeJSON(n));
        return out;
    },

    async stat(a) {
        return nodeJSON(await client.getNode(a.uid));
    },

    async download(a, id, signal) {
        const fd = fs.openSync(a.path, 'w', 0o600);
        let bytes = 0;
        let lastSent = 0;
        try {
            const dl = await client.getFileDownloader(a.uid, signal);
            const sink = new WritableStream<Uint8Array>({
                write(chunk) {
                    fs.writeSync(fd, chunk, 0, chunk.length, bytes);
                    bytes += chunk.length;
                    if (bytes - lastSent >= 256 * 1024) {
                        lastSent = bytes;
                        send({ id, event: 'progress', bytes });
                    }
                },
            });
            await dl.downloadToStream(sink).completion();
        } finally {
            fs.closeSync(fd);
        }
        return { size: bytes };
    },

    async upload(a, _id, signal) {
        const size = fs.statSync(a.path).size;
        const meta = {
            mediaType: mimeOf(a.name),
            expectedSize: size,
            modificationTime: a.mtime ? new Date(a.mtime) : undefined,
        };
        const uploader = a.uid
            ? await client.getFileRevisionUploader(a.uid, meta, signal)
            : await client.getFileUploader(a.parentUid, a.name, meta, signal);
        const stream = Readable.toWeb(fs.createReadStream(a.path)) as unknown as ReadableStream;
        const ctrl = await uploader.uploadFromStream(stream, []);
        const { nodeUid, nodeRevisionUid } = await ctrl.completion();
        return { uid: nodeUid, revUid: nodeRevisionUid, size };
    },

    async mkdir(a) {
        return nodeJSON(await client.createFolder(a.parentUid, a.name));
    },

    async rename(a) {
        await client.renameNode(a.uid, a.name);
        return null;
    },

    async move(a) {
        await results(client.moveNodes([a.uid], a.parentUid));
        return null;
    },

    async trash(a) {
        await results(client.trashNodes([a.uid]));
        return null;
    },

    async delete(a) {
        await results(client.deleteNodes([a.uid]));
        return null;
    },

    async quota() {
        return account.getQuota();
    },

    async cancel(a) {
        inflight.get(a.target)?.abort();
        return null;
    },
};

const rl = createInterface({ input: process.stdin });
rl.on('line', (line) => {
    if (!line.trim()) return;
    let req: { id: number; op: string; args?: Args };
    try { req = JSON.parse(line); } catch { return; }
    const fn = ops[req.op];
    if (!fn) return send({ id: req.id, ok: false, code: 'ENOSYS', error: `unknown op ${req.op}` });
    const ac = new AbortController();
    inflight.set(req.id, ac);
    fn(req.args ?? {}, req.id, ac.signal).then(
        (result) => send({ id: req.id, ok: true, result }),
        (e) => {
            const code = errCode(e);
            if (code !== 'ENOENT') console.error(`[bridge] ${req.op} failed: ${e?.message ?? e}`);
            send({ id: req.id, ok: false, code, error: String(e?.message ?? e) });
        },
    ).finally(() => {
        inflight.delete(req.id);
        if (closing && inflight.size === 0) process.exit(0);
    });
});
let closing = false;
rl.on('close', () => {
    closing = true;
    if (inflight.size === 0) process.exit(0);
});

send({ event: 'ready' });
