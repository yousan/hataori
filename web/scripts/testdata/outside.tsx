// Every line marked "want" has a user-visible string outside the catalog,
// and only those. CI runs the lint on this file with --expect-fail.
declare function t(key: string): string;
export function Outside(props: { busy: boolean; el: HTMLElement }) {
  document.title = "Dashboard"; // want
  props.el.textContent = `${props.busy} threads`; // want
  alert("Stopped"); // want
  props.el.className = "not shown";
  const message = "Saved"; // want
  const label = props.busy ? t("busy") : ("Ready" as const); // want
  const css = "row";
  return (
    <div
      title="Threads" // want
      aria-label={props.busy ? "Busy" : t("idle")} // want
      className={css}
      aria-description={label}
      data-id="7"
    >
      Hello {/* want */}
      {"world" /* want */}
      {`and ${props.busy}` /* want */}
      {props.busy && "Loading" /* want */}
      {message}
      {t("fine")}
      <img alt={t("logo")} src="/x.png" />
    </div>
  );
}
