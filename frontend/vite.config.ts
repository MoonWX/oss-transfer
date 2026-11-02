import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { execFileSync } from "node:child_process";
import { buildCatalog } from "./src/locales/index.ts";

let commit = process.env.VITE_BUILD_COMMIT || "unknown";
try {
  if (!process.env.VITE_BUILD_COMMIT)
    commit = execFileSync("git", ["rev-parse", "--short", "HEAD"], {
      cwd: import.meta.dirname,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
} catch {
  // Source archives can be built without a Git checkout.
}

// Serves each language's translations as its own module, so the application
// bundle carries neither table and a browser fetches only the one it shows.
// Vite restarts the dev server when a locale source changes.
function localeCatalogs(): Plugin {
  const prefix = "virtual:locale-catalog/";
  return {
    name: "locale-catalogs",
    resolveId: (id) => (id.startsWith(prefix) ? "\0" + id : undefined),
    load(id) {
      if (!id.startsWith("\0" + prefix)) return;
      const language = id.slice(prefix.length + 1);
      if (language !== "en" && language !== "ja")
        this.error(`Unknown locale catalog: ${language}`);
      // JSON.parse of a string literal is faster than an object literal.
      const json = JSON.stringify(buildCatalog(language));
      return `export default JSON.parse(${JSON.stringify(json)});`;
    },
  };
}

export default defineConfig({
  define: {
    __BUILD_INFO__: JSON.stringify({
      commit,
      builtAt: process.env.VITE_BUILD_TIME || new Date().toISOString(),
    }),
  },
  plugins: [react(), localeCatalogs()],
  base: "/",
  server: {
    port: 5173,
    proxy: { "/api": process.env.VITE_API_TARGET || "http://127.0.0.1:1685" },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 800,
    rolldownOptions: {
      output: {
        // React and the router change only when dependencies are upgraded,
        // so they keep their own chunk across application releases. MUI is
        // left to the default splitting so pages load only the parts they use.
        codeSplitting: {
          groups: [
            {
              name: "react",
              test: /node_modules[\\/](react|react-dom|react-router|react-router-dom|scheduler|cookie|set-cookie-parser)[\\/]/,
            },
          ],
        },
      },
    },
  },
});
