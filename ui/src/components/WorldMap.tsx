import { useMemo, useRef, useState } from "preact/hooks";
import {
  MARKER_RADIUS,
  NEUTRAL_FILL,
  RAMP_MIX,
  WORLD_UNCODED,
  WORLD_VIEWBOX,
  buildChoropleth,
  fillForStep,
  type CountryDatum,
} from "../utils/worldMap.js";

interface Props {
  data: CountryDatum[];
}

interface Hover {
  name: string;
  visitors: number;
  left: number;
  top: number;
}

const UNMATCHED_SHOWN = 4;

/**
 * Choropleth of visitors by country over real Natural Earth geometry. The
 * viewBox is degrees of longitude and latitude, so the SVG itself performs the
 * equirectangular projection and the paths are used exactly as generated.
 */
export default function WorldMap({ data }: Props) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const [hover, setHover] = useState<Hover | null>(null);

  const map = useMemo(() => buildChoropleth(data), [data]);

  const show = (name: string, visitors: number, e: MouseEvent) => {
    const rect = wrapRef.current?.getBoundingClientRect();
    if (!rect) return;
    // Keep the label inside the card, so a country at either edge is still
    // readable.
    const left = Math.min(Math.max(e.clientX - rect.left, 70), Math.max(rect.width - 70, 70));
    setHover({ name, visitors, left, top: e.clientY - rect.top });
  };

  const overflow = map.unmatched.length - UNMATCHED_SHOWN;

  return (
    <div class="obs-worldmap" ref={wrapRef} onMouseLeave={() => setHover(null)}>
      <svg viewBox={WORLD_VIEWBOX} preserveAspectRatio="xMidYMid meet" role="img"
        aria-label={`Visitors by country across ${map.shapes.length} countries`}>
        {/* Land Natural Earth carries with no ISO alpha-2 of its own. It can
            never take a fill, so it is drawn once, behind and inert. */}
        {WORLD_UNCODED.map(([name, d]) => (
          <path key={name} d={d} fill={NEUTRAL_FILL} />
        ))}

        {map.shapes.map((s) => (
          <path key={s.code} class="obs-worldmap-hit" d={s.d} fill={fillForStep(s.step)}
            onMouseMove={(e: MouseEvent) => show(s.name, s.visitors, e)} />
        ))}

        {/* Countries with no polygon at 1:110m — Singapore, Malta, Bahrain.
            Drawn at their Natural Earth point so a real count is never
            invisible. */}
        {map.markers.map((m) => (
          <circle key={m.code} class="obs-worldmap-hit" cx={m.x} cy={m.y} r={MARKER_RADIUS}
            fill={fillForStep(m.step)}
            onMouseMove={(e: MouseEvent) => show(m.name, m.visitors, e)} />
        ))}
      </svg>

      {hover && (
        <div class="obs-worldmap-tip" style={{ left: `${hover.left}px`, top: `${hover.top}px` }}>
          <div style="font-weight:600;">{hover.name}</div>
          <div style="color:var(--obs-text-secondary);font-variant-numeric:tabular-nums;">
            {hover.visitors.toLocaleString()} visitor{hover.visitors === 1 ? "" : "s"}
          </div>
        </div>
      )}

      <div class="obs-worldmap-footer">
        <div class="obs-worldmap-legend">
          <span>0</span>
          <i style={{ background: NEUTRAL_FILL }} />
          {RAMP_MIX.map((_, i) => (
            <i key={i} style={{ background: fillForStep(i + 1) }} />
          ))}
          <span>{map.max.toLocaleString()}</span>
        </div>

        {map.unmatched.length > 0 && (
          <div class="obs-worldmap-note"
            title={map.unmatched.map((u) => `${u.code}: ${u.visitors.toLocaleString()}`).join(", ")}>
            {map.unmatchedVisitors.toLocaleString()} visitor{map.unmatchedVisitors === 1 ? "" : "s"} not on the map (
            {map.unmatched.slice(0, UNMATCHED_SHOWN).map((u) => u.code).join(", ")}
            {overflow > 0 ? `, +${overflow} more` : ""})
          </div>
        )}
      </div>
    </div>
  );
}
