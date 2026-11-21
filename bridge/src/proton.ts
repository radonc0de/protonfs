// Builds a ProtonDriveClient from the CLI's stored session.
import '@protontech/crypto/polyfill';
import { CryptoProxy } from '@protontech/crypto';
import { Api as CryptoApi } from '@protontech/crypto/proxy/endpoint/api.ts';
import { getSrp, getRandomSrpVerifier, computeKeyPassword, generateKeySalt } from '@protontech/crypto/srp';
import {
    MemoryCache,
    OpenPGPCryptoWithCryptoProxy,
    ProtonDriveClient,
    type ProtonDriveAccount,
    type ProtonDriveAccountAddress,
    type ProtonDriveHTTPClient,
    type ProtonDriveHTTPClientBlobRequest,
    type ProtonDriveHTTPClientJsonRequest,
} from '@protontech/drive-sdk';
import { API_BASE, Session } from './session.ts';

let cryptoReady = false;
function initCrypto() {
    if (cryptoReady) return;
    CryptoApi.init({});
    CryptoProxy.setEndpoint(new CryptoApi(), (e: any) => e.clearKeyStore());
    cryptoReady = true;
}

class HttpClient implements ProtonDriveHTTPClient {
    constructor(private session: Session) {}

    fetchJson(req: ProtonDriveHTTPClientJsonRequest) {
        let body = req.body as any;
        if (req.json !== undefined) {
            body = JSON.stringify(req.json);
            req.headers.set('Content-Type', 'application/json');
        }
        return this.send(req.url, req.method, req.headers, body, req.timeoutMs, req.signal);
    }

    fetchBlob(req: ProtonDriveHTTPClientBlobRequest) {
        return this.send(req.url, req.method, req.headers, req.body as any, req.timeoutMs, req.signal);
    }

    private async send(url: string, method: string, headers: Headers, body: any, timeoutMs: number, signal?: AbortSignal) {
        const full = /^https?:\/\//.test(url) ? url : `${API_BASE}/${url.replace(/^\//, '')}`;
        // Block storage URLs (absolute, different host) don't take session headers.
        const isApi = full.startsWith(API_BASE);
        for (let attempt = 0; ; attempt++) {
            const h = new Headers(headers);
            const token = this.session.accessToken;
            if (isApi) this.session.authHeaders(h);
            const signals = [AbortSignal.timeout(timeoutMs || 60_000)];
            if (signal) signals.push(signal);
            const res = await fetch(full, { method, headers: h, body, signal: AbortSignal.any(signals) });
            if (res.status === 401 && isApi && attempt === 0) {
                await this.session.refresh(token);
                continue;
            }
            return res;
        }
    }
}

/** Minimal core-API helper for account endpoints. */
async function apiGet(http: HttpClient, path: string): Promise<any> {
    const res = await http.fetchJson({ url: path, method: 'GET', headers: new Headers(), timeoutMs: 30_000 });
    const body: any = await res.json().catch(() => ({}));
    if (!res.ok || (body.Code && body.Code !== 1000)) {
        throw new Error(`GET ${path} failed: ${res.status} ${body.Error ?? ''}`);
    }
    return body;
}

class Account implements ProtonDriveAccount {
    private addresses?: Promise<ProtonDriveAccountAddress[]>;
    private publicKeyCache = new Map<string, Promise<{ keys: any[]; proton: boolean }>>();

    constructor(private http: HttpClient, private session: Session) {}

    private load(): Promise<ProtonDriveAccountAddress[]> {
        this.addresses ??= (async () => {
            const { User } = await apiGet(this.http, 'core/v4/users');
            const userKeys: any[] = [];
            for (const k of User.Keys ?? []) {
                try {
                    userKeys.push(await CryptoProxy.importPrivateKey({ armoredKey: k.PrivateKey, passphrase: this.session.userKeyPassword }));
                } catch { /* inactive/old key */ }
            }
            if (!userKeys.length) throw new Error('Could not decrypt any user key (stale session?). Run: proton-drive auth login');

            const { Addresses } = await apiGet(this.http, 'core/v4/addresses');
            const result: ProtonDriveAccountAddress[] = [];
            for (const a of (Addresses as any[]).sort((x, y) => x.Order - y.Order)) {
                const keys: { id: string; key: any }[] = [];
                let primaryKeyIndex = 0;
                for (const k of a.Keys ?? []) {
                    try {
                        let passphrase = this.session.userKeyPassword;
                        if (k.Token) {
                            const { data } = await CryptoProxy.decryptMessage({ armoredMessage: k.Token, decryptionKeys: userKeys });
                            passphrase = data as string;
                        }
                        const key = await CryptoProxy.importPrivateKey({ armoredKey: k.PrivateKey, passphrase });
                        if (k.Primary === 1) primaryKeyIndex = keys.length;
                        keys.push({ id: k.ID, key });
                    } catch { /* undecryptable key: skip */ }
                }
                if (keys.length) result.push({ email: a.Email, addressId: a.ID, primaryKeyIndex, keys, ...{ status: a.Status } } as any);
            }
            if (!result.length) throw new Error('No usable address keys');
            return result;
        })();
        return this.addresses;
    }

    async getOwnAddresses() { return this.load(); }

    async getQuota() {
        const { User } = await apiGet(this.http, 'core/v4/users');
        return { max: User.MaxDriveSpace ?? User.MaxSpace ?? 0, used: User.UsedDriveSpace ?? User.UsedSpace ?? 0 };
    }

    async getOwnPrimaryAddress() {
        const all = await this.load();
        return all.find((a: any) => a.status === 1) ?? all[0];
    }

    async getOwnAddress(emailOrId: string) {
        const needle = emailOrId.toLowerCase();
        const a = (await this.load()).find((x) => x.addressId === emailOrId || x.email.toLowerCase() === needle);
        if (!a) throw new Error(`No own address ${emailOrId}`);
        return a;
    }

    private fetchPublic(email: string, force = false) {
        if (force || !this.publicKeyCache.has(email)) {
            this.publicKeyCache.set(email, (async () => {
                const body = await apiGet(this.http, `core/v4/keys/all?Email=${encodeURIComponent(email)}`).catch(() => null);
                const raw = body?.Address?.Keys ?? [];
                const keys = [];
                for (const k of raw) {
                    try { keys.push(await CryptoProxy.importPublicKey({ armoredKey: k.PublicKey })); } catch {}
                }
                return { keys, proton: !!body && raw.length > 0 && body.ProtonMX !== false };
            })());
        }
        return this.publicKeyCache.get(email)!;
    }

    async hasProtonAccount(email: string) { return (await this.fetchPublic(email)).proton; }
    async getPublicKeys(email: string, forceRefresh?: boolean) { return (await this.fetchPublic(email, forceRefresh)).keys as any; }
}

const srpModule = {
    getSrp: async (version: number, modulus: string, serverEphemeral: string, salt: string, password: string) => {
        const r = await getSrp({ Version: version as any, Modulus: modulus, ServerEphemeral: serverEphemeral, Salt: salt } as any, { password } as any, version as any);
        return { expectedServerProof: r.expectedServerProof, clientProof: r.clientProof, clientEphemeral: r.clientEphemeral };
    },
    getSrpVerifier: async (_password: string): Promise<any> => {
        throw new Error('protonfs does not support password-protected public links');
    },
    computeKeyPassword: (password: string, salt: string) => computeKeyPassword(password, salt),
    generateKeySalt: () => generateKeySalt(),
};
void getRandomSrpVerifier;

const verbose = !!process.env.PROTONFS_DEBUG;
const telemetry = {
    getLogger: (name: string) => ({
        debug: (m: string) => { if (verbose) console.error(`[sdk:${name}]`, m); },
        info: (m: string) => { if (verbose) console.error(`[sdk:${name}]`, m); },
        warn: (m: string) => console.error(`[sdk:${name}] WARN`, m),
        error: (m: string, e?: unknown) => console.error(`[sdk:${name}] ERROR`, m, (e as any)?.message ?? e ?? ''),
    }),
    recordMetric: () => {},
};

export async function createClient(): Promise<{ client: ProtonDriveClient; account: Account }> {
    initCrypto();
    const session = await Session.load();
    const http = new HttpClient(session);
    const account = new Account(http, session);
    await account.getOwnPrimaryAddress(); // fail fast on auth/key problems
    const client = new ProtonDriveClient({
        telemetry: telemetry as any,
        httpClient: http,
        entitiesCache: new MemoryCache(),
        cryptoCache: new MemoryCache(),
        account,
        openPGPCryptoModule: new OpenPGPCryptoWithCryptoProxy(CryptoProxy as any),
        srpModule: srpModule as any,
    });
    return { client, account };
}
