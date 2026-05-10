import Dashboard from "../components/Dashboard.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

export default function Home() {
  // Site comes from the layout-level RouteFilterProvider so the SiteSwitcher
  // in the sidebar can swap it without a page reload.
  const { state } = useFilters();
  return <Dashboard siteId={state.siteId} />;
}
