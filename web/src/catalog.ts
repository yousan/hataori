// Every string the page shows is here, and only here (design, section 7.2).
// scripts/lint-strings.mjs fails on a user-visible string anywhere else.
//
// Another language is one more catalog of the type Catalog: the type makes
// it name every key that English has, and nothing else.

const en = {
  "app.title": "hataori dashboard",
  "app.readOnly": "read-only",
  "shell.empty": "Nothing to show here yet.",
  "theme.toLight": "Switch to the light theme",
  "theme.toDark": "Switch to the dark theme",
  "theme.light": "Light",
  "theme.dark": "Dark",
} as const;

export type Key = keyof typeof en;
export type Catalog = { readonly [K in Key]: string };

// lang is the language the page is shown in, and catalog its strings.
export const lang = "en";
const catalog: Catalog = en;

// t is the string of key in the page's language.
export function t(key: Key): string {
  return catalog[key];
}
