import type { ComponentChildren } from "preact";
import { useState } from "preact/hooks";

interface Tab {
  key: string;
  label: string;
  content: ComponentChildren;
}

interface Props {
  tabs: Tab[];
  defaultTab?: string;
}

export default function Tabs({ tabs, defaultTab }: Props) {
  const [active, setActive] = useState(defaultTab || tabs[0]?.key || "");
  const current = tabs.find(t => t.key === active);

  return (
    <div>
      <div class="obs-tabs-bar">
        {tabs.map(tab => (
          <button
            key={tab.key}
            class={`obs-tab ${active === tab.key ? "obs-tab--active" : ""}`}
            onClick={() => setActive(tab.key)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      <div class="obs-tab-content">
        {current?.content}
      </div>
    </div>
  );
}
