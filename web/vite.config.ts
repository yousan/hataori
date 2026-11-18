import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The built page is committed and embedded in the hataori binary
// (internal/dashboard/page), so building hataori needs no Node. CI builds it
// again and fails when the result differs.
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../internal/dashboard/page",
    emptyOutDir: true,
    assetsDir: "assets",
    // Everything is a file of the page's own server: no data: URLs, which
    // the page's Content-Security-Policy would refuse.
    assetsInlineLimit: 0,
    sourcemap: false,
    reportCompressedSize: false,
  },
});
