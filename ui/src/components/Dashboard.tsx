import { useFilters } from "../hooks/useFilters.js";
import { api } from "../api.js";
import StatsCards from "./StatsCards.js";
import TimeSeriesChart from "./TimeSeriesChart.js";
import DatePicker from "./DatePicker.js";
import FilterBar from "./FilterBar.js";
import TabbedBreakdownPanel from "./TabbedBreakdownPanel.js";
import CustomEventsPanel from "./CustomEventsPanel.js";
import OnboardingGuide from "./OnboardingGuide.js";
import WorldMapPanel from "./WorldMapPanel.js";
import "../styles/dashboard.css";

function ExportButton() {
  const { state } = useFilters();
  const handleExport = (format: string) => {
    const token = localStorage.getItem("obs_token");
    const url = `/api/v1/export?site_id=${state.siteId}&from=${state.from}&to=${state.to}&format=${format}${token ? `&token=${token}` : ""}`;
    window.open(url, "_blank");
  };
  return (
    <div style={{ position: "relative", display: "inline-block" }}>
      <button class="obs-btn obs-btn--sm" onClick={() => handleExport("csv")}
        title="Export data as CSV">
        Export
      </button>
    </div>
  );
}

function DashboardInner() {
  const { state } = useFilters();

  return (
    <div class="obs-dashboard">
      <header class="obs-header">
        <h1 class="obs-page-title">Dashboard</h1>
        <div style={{ display: "flex", alignItems: "center", gap: "8px" }}>
          <ExportButton />
          <DatePicker />
        </div>
      </header>

      <FilterBar />

      <OnboardingGuide siteId={state.siteId} />

      <StatsCards />

      <div class="obs-card-static">
        <TimeSeriesChart />
      </div>

      <div class="obs-grid-2">
        <TabbedBreakdownPanel
          title="Pages"
          tabs={[
            { label: "Path", fetchFn: api.pages, labelKey: "pathname", valueKey: "pageviews", filterKey: "pathname" },
            { label: "Entry", fetchFn: api.entryPages, labelKey: "pathname", valueKey: "visitors", filterKey: "pathname" },
            { label: "Exit", fetchFn: api.exitPages, labelKey: "pathname", valueKey: "visitors", filterKey: "pathname" },
          ]}
        />
        <TabbedBreakdownPanel
          title="Sources"
          tabs={[
            { label: "Referrers", fetchFn: api.referrers, labelKey: "referrer", valueKey: "visitors", filterKey: "referrer" },
            { label: "Channels", fetchFn: api.channels, labelKey: "channel", valueKey: "visitors", filterKey: "channel" },
            {
              label: "UTM Source",
              fetchFn: (s: string, f: string, t: string, l?: number, fl?: Record<string, string>) => api.utm(s, f, t, "source", l, fl),
              labelKey: "value",
              valueKey: "visitors",
              filterKey: "utm_source",
            },
          ]}
        />
      </div>

      <div class="obs-grid-2">
        <TabbedBreakdownPanel
          title="Technology"
          tabs={[
            { label: "Browsers", fetchFn: api.browsers, labelKey: "browser", valueKey: "visitors", filterKey: "browser" },
            { label: "OS", fetchFn: api.os, labelKey: "os", valueKey: "visitors", filterKey: "os" },
            { label: "Devices", fetchFn: api.devices, labelKey: "device", valueKey: "visitors", filterKey: "device" },
            { label: "Screens", fetchFn: api.screens, labelKey: "screen", valueKey: "visitors", filterKey: "screen" },
          ]}
        />
        <TabbedBreakdownPanel
          title="Location"
          tabs={[
            { label: "Countries", fetchFn: api.countries, labelKey: "country", valueKey: "visitors", filterKey: "country" },
            { label: "Languages", fetchFn: api.languages, labelKey: "language", valueKey: "visitors", filterKey: "language" },
          ]}
        />
      </div>

      <WorldMapPanel />

      <CustomEventsPanel />
    </div>
  );
}

interface Props {
  /** Optional override; default is to read from the surrounding RouteFilterProvider. */
  siteId?: string;
}

function Dashboard(_props: Props) {
  return <DashboardInner />;
}

Dashboard.displayName = "Dashboard";
export default Dashboard;
