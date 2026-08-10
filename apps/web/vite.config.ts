import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    // The dev server never binds a public interface. In every deployed
    // environment the origin is reached only through Cloudflare Tunnel.
    host: "127.0.0.1",
    port: 5173,
    proxy: {
      "/v1": "http://127.0.0.1:8080",
      "/health": "http://127.0.0.1:8080",
    },
  },
  build: { outDir: "dist", sourcemap: true },
});
