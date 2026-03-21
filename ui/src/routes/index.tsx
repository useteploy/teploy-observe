import { Island } from "neutron/client";
import Dashboard from "../components/Dashboard.js";

export const config = { mode: "app" };

export default function Home() {
  // Site ID from URL params or default
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  return <Island component={Dashboard} client="load" siteId={siteId} />;
}
