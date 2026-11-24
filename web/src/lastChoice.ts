// The last choice a person made for a thread's start, from the thread's
// last events.

import type { Event } from "./api";

// Chosen is a start_chosen event, and the first turn that started after it.
export interface Chosen {
  pos: number; // the event's
  data: { [field: string]: unknown }; // the event's
  turn: number; // the position of that turn_started event; 0 for none yet
}

// chosenOf is the choice that evs, a thread's last events in order, show,
// until the second turn after it starts; before is the one known until now,
// which stays when evs do not reach back to it.
export function chosenOf(evs: readonly Event[], before: Chosen | null): Chosen | null {
  let c = before;
  for (const e of evs) {
    if (e.kind === "start_chosen") {
      if (c === null || e.pos > c.pos) c = { pos: e.pos, data: e.data ?? {}, turn: 0 };
    } else if (e.kind === "turn_started" && c !== null && e.pos > c.pos && e.pos !== c.turn) {
      c = c.turn > 0 ? null : { ...c, turn: e.pos };
    }
  }
  return c;
}
