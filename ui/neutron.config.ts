import { defineConfig, adapterStatic } from "neutron";

export default defineConfig({
  runtime: "preact",
  adapter: adapterStatic({
    allowAppRoutes: true,
  }),
});
