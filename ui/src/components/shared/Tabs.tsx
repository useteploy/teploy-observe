import type { ComponentChildren } from "preact";
import { useState, useRef } from "preact/hooks";

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
  const tabRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const current = tabs.find(t => t.key === active);

  const handleKeyDown = (e: KeyboardEvent, idx: number) => {
    let next = -1;
    if (e.key === "ArrowRight") next = (idx + 1) % tabs.length;
    else if (e.key === "ArrowLeft") next = (idx - 1 + tabs.length) % tabs.length;
    if (next >= 0) {
      e.preventDefault();
      setActive(tabs[next].key);
      tabRefs.current[next]?.focus();
    }
  };

  return (
    <div>
      <div class="obs-tabs-bar" role="tablist">
        {tabs.map((tab, i) => (
          <button
            key={tab.key}
            ref={(el) => { tabRefs.current[i] = el; }}
            role="tab"
            aria-selected={active === tab.key}
            tabIndex={active === tab.key ? 0 : -1}
            class={`obs-tab ${active === tab.key ? "obs-tab--active" : ""}`}
            onClick={() => setActive(tab.key)}
            onKeyDown={(e) => handleKeyDown(e as unknown as KeyboardEvent, i)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      <div class="obs-tab-content" role="tabpanel">
        {current?.content}
      </div>
    </div>
  );
}
