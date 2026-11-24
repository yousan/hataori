// What a person chose for a start that files of the repository blocked, as
// a start_chosen event says (protocol 3.33).

import { t } from "./catalog";

// The choices, as the server takes them and the event names them.
export const withoutSettings = "without project settings";
export const trusted = "trusted";
export const closed = "closed";

// chosenLine says who chose what.
export function chosenLine(choice: string, who: string | undefined): string {
  const w = { who: who ?? t("conv.someone") };
  switch (choice) {
    case withoutSettings:
      return t("start.chose.without", w);
    case trusted:
      return t("start.chose.trusted", w);
    case closed:
      return t("start.chose.closed", w);
  }
  return t("start.chose.other", { ...w, choice });
}
