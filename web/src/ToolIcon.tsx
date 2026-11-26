// Drawn for the dashboard: a shared grid and stroke, with no icon dependency.
export type Tool = "list" | "filter" | "tree" | "split" | "details" | "sun" | "moon";

export function ToolIcon({ name }: { name: Tool }) {
  return (
    <svg className="tool-icon" width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      {name === "list" && <path d="m4 6 4 4 4-4" />}
      {name === "filter" && <path d="M2 3h12L9 9v4l-2 1V9Z" />}
      {(name === "tree" || name === "split" || name === "details") && <rect x="2" y="2" width="12" height="12" rx="1.5" />}
      {name === "tree" && <path d="M6 2v12M3.5 5h0M3.5 8h0" />}
      {name === "split" && <path d="M8 2v12" />}
      {name === "details" && <path d="M10 2v12M12 5h0M12 8h0" />}
      {name === "moon" && <path d="M13.5 9.5A6 6 0 0 1 6.5 2.5a6 6 0 1 0 7 7Z" />}
      {name === "sun" && <><circle cx="8" cy="8" r="3" /><path d="M8 1v1M8 14v1M1 8h1M14 8h1M3 3l.7.7M12.3 12.3l.7.7M3 13l.7-.7M12.3 3.7l.7-.7" /></>}
    </svg>
  );
}
