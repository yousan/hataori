export interface SetupInfo {
  existing: boolean;
  agents: { name: string; command: string }[];
}

// An ordinary native daemon never gains setup routes. A restricted daemon
// explicitly advertises setup before the page requests first-run metadata.
export async function readDesktopSetup(request: typeof fetch = fetch): Promise<SetupInfo | null> {
  const identity = await request("/api/native");
  if (!identity.ok) throw new Error("Native identity unavailable");
  const value = await identity.json() as { setup?: boolean };
  if (value.setup !== true) return null;
  const response = await request("/api/setup");
  if (!response.ok) throw new Error("Setup unavailable");
  const info = await response.json() as SetupInfo;
  if (!Array.isArray(info.agents) || info.agents.length > 16 || typeof info.existing !== "boolean" ||
      info.agents.some(a => typeof a.name !== "string" || !a.name || typeof a.command !== "string")) throw new Error("Invalid setup state");
  return info;
}
