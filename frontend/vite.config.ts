import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { execFileSync } from "node:child_process";

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

export default defineConfig({
  define: {
    __BUILD_INFO__: JSON.stringify({
      commit,
      builtAt: process.env.VITE_BUILD_TIME || new Date().toISOString(),
    }),
  },
  plugins: [react()],
  base: "/",
  server: {
    port: 5173,
    proxy: { "/api": process.env.VITE_API_TARGET || "http://127.0.0.1:1685" },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 800,
  },
});
