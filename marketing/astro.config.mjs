import { defineConfig } from "astro/config";

// Static export. No SSR. No JS islands required.
export default defineConfig({
  output: "static",
  site: "https://observe.dev",
  trailingSlash: "ignore",
  build: {
    inlineStylesheets: "auto",
    assets: "_assets",
  },
  compressHTML: true,
  devToolbar: { enabled: false },
});
