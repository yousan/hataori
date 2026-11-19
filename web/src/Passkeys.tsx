import { useEffect, useRef, useState } from "react";
import { type Key, t } from "./catalog";
import { when } from "./format";
import {
  addPasskey,
  listPasskeys,
  type Passkey,
  PasskeyError,
  type PasskeyFailure,
  passkeysHere,
  registerPasskey,
  removePasskey,
  signInWithPasskey,
} from "./passkeys";
import { dropProof, linkProofUntil, watchProof } from "./session";

const failureText: Record<PasskeyFailure, Key> = {
  cancelled: "passkey.cancelled",
  refused: "passkey.refused",
  needsProof: "passkey.needsProof",
  failed: "passkey.failed",
};

function failure(e: unknown): PasskeyFailure {
  return e instanceof PasskeyError ? e.why : "failed";
}

function Failure({ why }: { why: PasskeyFailure | null }) {
  return why ? (
    <p className="passkey-error" data-passkey-error={why}>
      {t(failureText[why])}
    </p>
  ) : null;
}

// useLinkProof reports whether a sign-in link just brought a proof that
// registers a passkey, and renders again when that changes or the proof
// expires.
function useLinkProof(): boolean {
  const [until, setUntil] = useState(linkProofUntil);
  const [now, setNow] = useState(Date.now);
  useEffect(() => watchProof(() => setUntil(linkProofUntil())), []);
  useEffect(() => {
    const left = until - Date.now();
    if (left <= 0) return;
    const timer = setTimeout(() => setNow(Date.now()), left + 50);
    return () => clearTimeout(timer);
  }, [until]);
  return until > now;
}

// SignInNotice says when the page is not signed in, or was opened with a
// link that did not sign in, and on the web listener offers signing in
// with a passkey, which asks for no name. Right after a sign-in link
// signed in there, it offers registering a passkey on this device.
export function SignInNotice({ signedOut, refused }: { signedOut: boolean; refused: boolean }) {
  const offered = useLinkProof();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<PasskeyFailure | null>(null);
  const [added, setAdded] = useState(false);
  const here = passkeysHere();

  const signIn = async () => {
    setBusy(true);
    setError(null);
    try {
      await signInWithPasskey();
      location.replace("/"); // the page starts again with the new session
    } catch (e) {
      setError(failure(e));
      setBusy(false);
    }
  };
  const register = async () => {
    setBusy(true);
    setError(null);
    try {
      await registerPasskey();
      setAdded(true);
    } catch (e) {
      setError(failure(e));
    } finally {
      setBusy(false);
    }
  };

  if (signedOut || refused) {
    return (
      <div className="signed-out" role="alert">
        <p>{t(refused ? "app.linkUsed" : "app.signedOut")}</p>
        {here && (
          <button type="button" data-passkey="signin" disabled={busy} onClick={() => void signIn()}>
            {t("passkey.signIn")}
          </button>
        )}
        <Failure why={error} />
      </div>
    );
  }
  if (!here || (!offered && !added)) return null;
  return (
    <div className="passkey-offer" role="status" data-passkey="offer">
      <p>{t(added ? "passkey.added" : "passkey.offer")}</p>
      {!added && (
        <button type="button" data-passkey="register" disabled={busy} onClick={() => void register()}>
          {t("passkey.register")}
        </button>
      )}
      <button
        type="button"
        data-passkey="dismiss"
        onClick={() => {
          dropProof();
          setAdded(false);
          setError(null);
        }}
      >
        {t(added ? "passkey.close" : "passkey.notNow")}
      </button>
      <Failure why={error} />
    </div>
  );
}

// Passkeys is the button that opens the person's passkeys: when each was
// made and last used, with a way to remove it, from either listener; on
// the web listener, also a way to add one on this device.
export function Passkeys() {
  const dialog = useRef<HTMLDialogElement>(null);
  const [list, setList] = useState<Passkey[] | null>(null);
  const [error, setError] = useState<PasskeyFailure | null>(null);
  const [busy, setBusy] = useState(false);
  const here = passkeysHere();

  const load = async () => {
    try {
      setList(await listPasskeys());
    } catch (e) {
      setError(failure(e));
    }
  };
  const step = async (f: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await f();
    } catch (e) {
      setError(failure(e));
    } finally {
      setBusy(false);
      await load();
    }
  };
  const open = () => {
    setError(null);
    setList(null);
    dialog.current?.showModal();
    void load();
  };

  return (
    <>
      <button type="button" className="passkeys-open" data-tool="passkeys" onClick={open}>
        {t("passkey.list")}
      </button>
      <dialog ref={dialog} className="passkeys" aria-label={t("passkey.title")}>
        <h2>{t("passkey.title")}</h2>
        {list === null ? (
          <p className="muted">{t("passkey.loading")}</p>
        ) : list.length === 0 ? (
          <p className="none" data-passkeys="none">
            {t("passkey.none")}
          </p>
        ) : (
          <ul>
            {list.map((k) => (
              <li key={k.id} data-passkey-id={k.id}>
                <span>{t("passkey.made", { time: when(k.made) })}</span>
                <span className="muted">{k.used ? t("passkey.used", { time: when(k.used) }) : t("passkey.neverUsed")}</span>
                <button type="button" data-passkey="remove" disabled={busy} onClick={() => void step(() => removePasskey(k.id))}>
                  {t("passkey.remove")}
                </button>
              </li>
            ))}
          </ul>
        )}
        {!here && <p className="muted">{t("passkey.webOnly")}</p>}
        <Failure why={error} />
        <div className="passkeys-actions">
          {here && (
            <button type="button" data-passkey="add" disabled={busy} onClick={() => void step(addPasskey)}>
              {t("passkey.add")}
            </button>
          )}
          <button type="button" data-passkey="close" onClick={() => dialog.current?.close()}>
            {t("passkey.close")}
          </button>
        </div>
      </dialog>
    </>
  );
}
