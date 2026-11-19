// Passkeys on the web listener (internal/dashboard, passkeys.go): signing
// in with one, registering one with the proof a sign-in brought, listing
// the person's passkeys and removing one.
//
// The options the server sends and the credential the page sends back are
// in WebAuthn's JSON forms, binary values in base64url; the page converts
// them itself rather than rely on PublicKeyCredential.parse*FromJSON,
// which not every phone's browser has yet.

import { adopt, authorized, dropProof, registerProof, type SignedIn } from "./session";

const signInBegin = "/api/passkeys/signin/begin";
const signInFinish = "/api/passkeys/signin/finish";
const registerBegin = "/api/passkeys/register/begin";
const registerFinish = "/api/passkeys/register/finish";
const passkeysPath = "/api/passkeys";

// Passkey is one of the person's passkeys as the server lists it.
export interface Passkey {
  id: string;
  made: string;
  used?: string;
}

// PasskeyFailure is why a passkey step did not happen, for the page to say.
export type PasskeyFailure = "cancelled" | "refused" | "needsProof" | "failed";

export class PasskeyError extends Error {
  constructor(readonly why: PasskeyFailure) {
    super(why);
  }
}

// passkeysHere reports whether this page can sign in with and register
// passkeys: the web listener is the one served over HTTPS (the loopback
// listener is plain HTTP), in a browser that has WebAuthn.
export function passkeysHere(): boolean {
  return location.protocol === "https:" && typeof PublicKeyCredential !== "undefined" && !!navigator.credentials;
}

function fromB64(s: string): ArrayBuffer {
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(s.length / 4) * 4, "="));
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

function toB64(b: ArrayBuffer | ArrayBufferView): string {
  const bytes = b instanceof ArrayBuffer ? new Uint8Array(b) : new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
  let bin = "";
  for (const x of bytes) bin += String.fromCharCode(x);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

type Descriptor = { type: "public-key"; id: string; transports?: string[] };

function descriptors(list: Descriptor[] | undefined): PublicKeyCredentialDescriptor[] | undefined {
  return list?.map((d) => ({ type: d.type, id: fromB64(d.id), transports: d.transports as AuthenticatorTransport[] | undefined }));
}

// send posts body as JSON, with the session when signed in, and returns
// the answer's JSON. A failure becomes a PasskeyError.
async function send<T>(path: string, body: unknown, refusal: PasskeyFailure): Promise<T> {
  let r: Response;
  try {
    r = await fetch(path, authorized({ method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }));
  } catch {
    throw new PasskeyError("failed");
  }
  if (r.status === 401 || r.status === 403) throw new PasskeyError(refusal);
  if (!r.ok) throw new PasskeyError("failed");
  return (await r.json()) as T;
}

// ask runs a browser's passkey prompt; the person closing it is
// "cancelled".
async function ask<T>(f: () => Promise<T>): Promise<T> {
  try {
    return await f();
  } catch (e) {
    if (e instanceof DOMException && (e.name === "NotAllowedError" || e.name === "AbortError")) throw new PasskeyError("cancelled");
    throw new PasskeyError("failed");
  }
}

interface Begun<O> {
  ceremony: string;
  options: { publicKey: O };
}

// AssertionOptions is a check's options as the server sends them.
export type AssertionOptions = Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> & {
  challenge: string;
  allowCredentials?: Descriptor[];
};

// passkeyAnswer runs the browser's passkey prompt for a check's options
// and returns the credential in its JSON form, as the server takes it.
export async function passkeyAnswer(o: AssertionOptions): Promise<unknown> {
  const cred = (await ask(() =>
    navigator.credentials.get({ publicKey: { ...o, challenge: fromB64(o.challenge), allowCredentials: descriptors(o.allowCredentials) } }),
  )) as PublicKeyCredential | null;
  if (!cred) throw new PasskeyError("cancelled");
  const a = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: toB64(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    response: {
      clientDataJSON: toB64(a.clientDataJSON),
      authenticatorData: toB64(a.authenticatorData),
      signature: toB64(a.signature),
      userHandle: a.userHandle ? toB64(a.userHandle) : undefined,
    },
    clientExtensionResults: {},
  };
}

// signInWithPasskey signs in with any passkey of this dashboard: no name
// is asked for. With check, it is a check of the signed-in person's own
// passkey instead, and only theirs are offered. The session it opens
// replaces the page's, and it brings a proof that registers one more
// passkey.
export async function signInWithPasskey(check = false): Promise<void> {
  const b = await send<Begun<AssertionOptions>>(signInBegin, check ? { check: true } : {}, "refused");
  const credential = await passkeyAnswer(b.options.publicKey);
  const answer = await send<SignedIn>(signInFinish, { ceremony: b.ceremony, credential }, "refused");
  if (!adopt(answer, "passkey")) throw new PasskeyError("failed");
}

// registerPasskey registers a passkey on this device with the proof the
// last sign-in brought, which it uses up.
export async function registerPasskey(): Promise<void> {
  const proof = registerProof();
  if (!proof) throw new PasskeyError("needsProof");
  type Creation = Omit<PublicKeyCredentialCreationOptions, "challenge" | "user" | "excludeCredentials"> & {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: Descriptor[];
  };
  const b = await send<Begun<Creation>>(registerBegin, { register: proof }, "needsProof");
  const o = b.options.publicKey;
  const cred = (await ask(() =>
    navigator.credentials.create({
      publicKey: { ...o, challenge: fromB64(o.challenge), user: { ...o.user, id: fromB64(o.user.id) }, excludeCredentials: descriptors(o.excludeCredentials) },
    }),
  )) as PublicKeyCredential | null;
  if (!cred) throw new PasskeyError("cancelled");
  const a = cred.response as AuthenticatorAttestationResponse;
  const ext = cred.getClientExtensionResults();
  await send<unknown>(
    registerFinish,
    {
      ceremony: b.ceremony,
      credential: {
        id: cred.id,
        rawId: toB64(cred.rawId),
        type: cred.type,
        authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
        response: {
          clientDataJSON: toB64(a.clientDataJSON),
          attestationObject: toB64(a.attestationObject),
          transports: a.getTransports?.() ?? [],
        },
        // Only what was asked for: the server refuses other outputs.
        clientExtensionResults: ext.credProps ? { credProps: ext.credProps } : {},
      },
    },
    "needsProof",
  );
  dropProof();
}

// addPasskey registers a passkey on this device: with the proof of the
// sign-in just made, else after checking a passkey the person has, which
// brings a proof of its own.
export async function addPasskey(): Promise<void> {
  if (!registerProof()) {
    try {
      await signInWithPasskey(true);
    } catch (e) {
      // No passkey here to check, or the person closed the prompt: a link is the other way.
      throw e instanceof PasskeyError && e.why !== "failed" ? new PasskeyError("needsProof") : e;
    }
  }
  await registerPasskey();
}

// listPasskeys returns the person's passkeys.
export async function listPasskeys(): Promise<Passkey[]> {
  const r = await fetch(passkeysPath, authorized());
  if (r.status === 401) throw new PasskeyError("refused");
  if (!r.ok) throw new PasskeyError("failed");
  return (await r.json()) as Passkey[];
}

// removePasskey removes one of the person's passkeys.
export async function removePasskey(id: string): Promise<void> {
  let r: Response;
  try {
    r = await fetch(`${passkeysPath}/${encodeURIComponent(id)}`, authorized({ method: "DELETE" }));
  } catch {
    throw new PasskeyError("failed");
  }
  if (r.status === 401) throw new PasskeyError("refused");
  if (!r.ok && r.status !== 404) throw new PasskeyError("failed");
}
