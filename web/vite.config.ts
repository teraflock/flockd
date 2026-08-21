import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Dev server proxies to a locally running flockd so `npm run dev` works
// against the real daemon API.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": "http://127.0.0.1:7777",
      "/v1": "http://127.0.0.1:7777",
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
