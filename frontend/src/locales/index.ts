import { comparisonMessages } from "./comparison.ts";
import { coreMessages } from "./core.ts";
import { adminMessages } from "./admin.ts";
import { tasksMessages } from "./tasks.ts";
import { updatesMessages } from "./updates.ts";

export type Message = { en: string; ja: string };
export type Catalog = Record<string, string>;

// Later catalogs win when a source string appears twice.
export const sourceCatalogs: Record<string, Message>[] = [
  updatesMessages,
  tasksMessages,
  comparisonMessages,
  adminMessages,
  coreMessages,
];

/** Flattens the source catalogs into the table for one language. */
export function buildCatalog(language: "en" | "ja"): Catalog {
  const catalog: Catalog = Object.create(null);
  for (const source of sourceCatalogs)
    for (const [key, message] of Object.entries(source))
      catalog[key] = message[language];
  return catalog;
}
