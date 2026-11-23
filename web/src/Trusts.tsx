import { useRef, useState } from "react";
import { trusts, type TrustedFile } from "./api";
import { t } from "./catalog";
import { when } from "./format";

// Trusts is the button that opens the files the owner trusted from a
// blocked start (#590): each with its scope, repository, who trusted it
// and when, and its number, which `hataori untrust` takes on the host. The
// page itself changes nothing.
export function Trusts() {
  const dialog = useRef<HTMLDialogElement>(null);
  const [list, setList] = useState<TrustedFile[] | null>(null);
  const [failed, setFailed] = useState(false);

  const open = () => {
    setList(null);
    setFailed(false);
    dialog.current?.showModal();
    trusts().then(setList, () => setFailed(true));
  };

  return (
    <>
      <button type="button" className="passkeys-open" data-tool="trusts" onClick={open}>
        {t("trusts.list")}
      </button>
      <dialog ref={dialog} className="passkeys trusts" aria-label={t("trusts.title")}>
        <h2>{t("trusts.title")}</h2>
        {failed ? (
          <p className="passkey-error">{t("trusts.failed")}</p>
        ) : list === null ? (
          <p className="muted">{t("trusts.loading")}</p>
        ) : list.length === 0 ? (
          <p className="none" data-trusts="none">
            {t("trusts.none")}
          </p>
        ) : (
          <ul>
            {list.map((x) => (
              <li key={x.id} data-trust-id={x.id}>
                <span>{t("trusts.file", { file: x.file, repo: x.repo })}</span>
                <span className="muted">{t("trusts.by", { n: x.id, scope: x.scope, by: x.by, time: when(x.at) })}</span>
              </li>
            ))}
          </ul>
        )}
        <p className="muted">{t("trusts.withdraw")}</p>
        <div className="passkeys-actions">
          <button type="button" data-trusts="close" onClick={() => dialog.current?.close()}>
            {t("passkey.close")}
          </button>
        </div>
      </dialog>
    </>
  );
}
