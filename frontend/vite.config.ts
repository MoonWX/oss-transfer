import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { rmSync } from "node:fs";
import { resolve } from "node:path";
import { execFileSync } from "node:child_process";

let commit = "unknown";
try {
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
      builtAt: new Date().toISOString(),
    }),
  },
  plugins: [
    react(),
    {
      name: "clean-generated-assets",
      apply: "build",
      buildStart() {
        rmSync(
          resolve(import.meta.dirname, "../src/main/resources/static/assets"),
          { recursive: true, force: true },
        );
        rmSync(
          resolve(import.meta.dirname, "../src/main/resources/static/pages"),
          { recursive: true, force: true },
        );
      },
    },
  ],
  base: "/",
  server: {
    port: 5173,
    proxy: { "/api": process.env.VITE_API_TARGET || "http://127.0.0.1:1685" },
  },
  build: {
    outDir: "../src/main/resources/static",
    emptyOutDir: false,
    chunkSizeWarningLimit: 800,
  },
});
