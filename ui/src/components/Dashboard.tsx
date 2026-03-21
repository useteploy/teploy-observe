import { FilterProvider, useFilters } from "../hooks/useFilters.js";
import { api } from "../api.js";
import StatsCards from "./StatsCards.js";
import TimeSeriesChart from "./TimeSeriesChart.js";
import DatePicker from "./DatePicker.js";
import FilterBar from "./FilterBar.js";
import TabbedBreakdownPanel from "./TabbedBreakdownPanel.js";
import CustomEventsPanel from "./CustomEventsPanel.js";
import "../styles/dashboard.css";

function DashboardInner() {
  return (
    <div class="obs-dashboard">
      <header class="obs-header">
        <div class="obs-header-brand">
          <div class="obs-header-logo">O</div>
          <h1>Observe</h1>
        </div>
        <DatePicker />
      </header>

      <FilterBar />

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

      <CustomEventsPanel />
    </div>
  );
}

interface Props {
  siteId: string;
}

function Dashboard({ siteId }: Props) {
  return (
    <FilterProvider siteId={siteId}>
      <DashboardInner />
    </FilterProvider>
  );
}

Dashboard.displayName = "Dashboard";
export default Dashboard;
