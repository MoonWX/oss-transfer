// Node-side tests translate synchronously, without the browser's lazy chunks.
import { buildCatalog } from "../src/locales/index.ts";
import { registerCatalog } from "../src/lib/i18n";

registerCatalog("en", buildCatalog("en"));
registerCatalog("ja", buildCatalog("ja"));
