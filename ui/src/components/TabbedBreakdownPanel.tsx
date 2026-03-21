import { useState } from "preact/hooks";
import BreakdownTable from "./BreakdownTable.js";

interface Tab {
  label: string;
  fetchFn: (siteId: string, from: string, to: string, limit?: number, filters?: Record<string, string>) => Promise<any[]>;
  labelKey: string;
  valueKey: string;
  filterKey: string;
}

interface Props {
  title: string;
  tabs: Tab[];
}

function TabbedBreakdownPanel({ title, tabs }: Props) {
  const [activeIdx, setActiveIdx] = useState(0);
  const activeTab = tabs[activeIdx];

  return (
    <div class="obs-card-static">
      <h3 class="obs-section-title" style="margin-bottom:12px;">{title}</h3>
      {tabs.length > 1 && (
        <div class="obs-tabs">
          {tabs.map((tab, i) => (
            <button
              key={tab.label}
              class={`obs-tab ${i === activeIdx ? "obs-tab-active" : ""}`}
              onClick={() => setActiveIdx(i)}
            >
              {tab.label}
            </button>
          ))}
        </div>
      )}
      <BreakdownTable
        key={activeTab.label}
        fetchFn={activeTab.fetchFn}
        labelKey={activeTab.labelKey}
        valueKey={activeTab.valueKey}
        filterKey={activeTab.filterKey}
      />
    </div>
  );
}

TabbedBreakdownPanel.displayName = "TabbedBreakdownPanel";
export default TabbedBreakdownPanel;
