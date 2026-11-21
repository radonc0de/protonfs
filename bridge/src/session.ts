// Reuses the session that `proton-drive auth login` stores in the system keyring
// (libsecret, service=ch.proton.drive/drive-sdk-cli, account=auth-session).
// Refreshed tokens are written back so the official CLI keeps working.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { readFileSync, existsSync } from 'node:fs';
import { delimiter, join } from 'node:path';

const run = promisify(execFile);

export const API_BASE = 'https://drive-api.proton.me';

// The session belongs to the official CLI, so identify as that CLI. Read the
// version string out of the installed binary so CLI updates don't break us.
function detectAppVersion(): string {
    if (process.env.PROTONFS_APPVERSION) return process.env.PROTONFS_APPVERSION;
    const candidates = [process.env.PROTON_DRIVE_BIN, ...(process.env.PATH ?? '').split(delimiter).map((d) => join(d, 'proton-drive'))];
    for (const c of candidates) {
        if (!c || !existsSync(c)) continue;
        try {
            const m = readFileSync(c).toString('latin1').match(/cli-drive@\d+\.\d+\.\d+(?:\+[0-9a-f]+)?/);
            if (m) return m[0];
        } catch {}
    }
    return 'cli-drive@0.9.0+75612299';
}
export const APP_VERSION = detectAppVersion();

const ATTRS = ['service', 'ch.proton.drive/drive-sdk-cli', 'account', 'auth-session'];
const SCHEMA = ['xdg:schema', 'com.oven-sh.bun.Secret'];

interface StoredSecret {
    session: { uid: string; accessToken: string; refreshToken: string };
    userKeyPassword: string;
    [k: string]: unknown;
}

export class Session {
    private secret!: StoredSecret;
    private refreshing?: Promise<void>;

    static async load(): Promise<Session> {
        const s = new Session();
        s.secret = await readSecret();
        return s;
    }

    get uid() { return this.secret.session.uid; }
    get accessToken() { return this.secret.session.accessToken; }
    get userKeyPassword() { return this.secret.userKeyPassword; }

    authHeaders(h: Headers) {
        h.set('x-pm-uid', this.uid);
        h.set('Authorization', `Bearer ${this.accessToken}`);
        h.set('x-pm-appversion', APP_VERSION);
        if (!h.has('Accept')) h.set('Accept', 'application/vnd.protonmail.v1+json');
    }

    /** Refresh tokens once, even if many requests hit 401 concurrently. */
    refresh(failedAccessToken: string): Promise<void> {
        if (failedAccessToken !== this.accessToken) return Promise.resolve(); // already refreshed
        if (!this.refreshing) {
            this.refreshing = this.doRefresh().finally(() => { this.refreshing = undefined; });
        }
        return this.refreshing;
    }

    private async doRefresh() {
        // The CLI may have refreshed in the meantime; pick that up first.
        const latest = await readSecret();
        if (latest.session.accessToken !== this.secret.session.accessToken) {
            this.secret = latest;
            return;
        }
        const res = await fetch(`${API_BASE}/auth/v4/refresh`, {
            method: 'POST',
            headers: {
                'x-pm-uid': this.uid,
                'x-pm-appversion': APP_VERSION,
                'Content-Type': 'application/json',
                Accept: 'application/vnd.protonmail.v1+json',
            },
            body: JSON.stringify({
                UID: this.uid,
                RefreshToken: this.secret.session.refreshToken,
                ResponseType: 'token',
                GrantType: 'refresh_token',
                RedirectURI: 'https://protonmail.com',
            }),
        });
        const body: any = await res.json().catch(() => ({}));
        if (!res.ok || !body.AccessToken) {
            // Another protonfs instance or the CLI may have rotated the token first.
            const again = await readSecret();
            if (again.session.refreshToken !== this.secret.session.refreshToken) {
                this.secret = again;
                return;
            }
            throw new Error(`Session refresh failed (${res.status} ${body.Error ?? ''}). Run: proton-drive auth login`);
        }
        this.secret = {
            ...this.secret,
            session: {
                uid: body.UID ?? this.uid,
                accessToken: body.AccessToken,
                refreshToken: body.RefreshToken,
            },
        };
        await writeSecret(this.secret);
    }
}

async function readSecret(): Promise<StoredSecret> {
    let out: string;
    try {
        ({ stdout: out } = await run('secret-tool', ['lookup', ...ATTRS]));
    } catch {
        throw new Error('No Proton Drive session in keyring. Run: proton-drive auth login');
    }
    const s = JSON.parse(out) as StoredSecret;
    if (!s?.session?.uid || !s.userKeyPassword) throw new Error('Unexpected session format in keyring');
    return s;
}

async function writeSecret(s: StoredSecret) {
    await new Promise<void>((resolve, reject) => {
        const p = execFile(
            'secret-tool',
            ['store', '--label=ch.proton.drive/drive-sdk-cli/auth-session', ...SCHEMA, ...ATTRS],
            (err) => (err ? reject(err) : resolve()),
        );
        p.stdin!.end(JSON.stringify(s));
    });
}
