import { useState } from "preact/hooks";

interface CountryData {
  country: string;
  visitors: number;
}

interface Props {
  data: CountryData[];
}

// Approximate lat/lon → 800x400 SVG coordinates
// Format: [countryCode, x, y, displayName]
const COUNTRY_POSITIONS: Array<[string, number, number, string]> = [
  ["US", 180, 170, "United States"],
  ["CA", 200, 130, "Canada"],
  ["MX", 170, 210, "Mexico"],
  ["BR", 290, 260, "Brazil"],
  ["AR", 270, 320, "Argentina"],
  ["CL", 255, 310, "Chile"],
  ["CO", 245, 225, "Colombia"],
  ["PE", 250, 265, "Peru"],
  ["GB", 400, 140, "United Kingdom"],
  ["IE", 385, 140, "Ireland"],
  ["FR", 410, 160, "France"],
  ["DE", 425, 150, "Germany"],
  ["ES", 400, 175, "Spain"],
  ["IT", 430, 175, "Italy"],
  ["NL", 420, 145, "Netherlands"],
  ["BE", 415, 148, "Belgium"],
  ["SE", 435, 115, "Sweden"],
  ["NO", 425, 105, "Norway"],
  ["FI", 450, 105, "Finland"],
  ["DK", 425, 130, "Denmark"],
  ["PL", 450, 150, "Poland"],
  ["CH", 420, 165, "Switzerland"],
  ["AT", 435, 160, "Austria"],
  ["CZ", 445, 155, "Czechia"],
  ["PT", 390, 180, "Portugal"],
  ["GR", 460, 190, "Greece"],
  ["TR", 485, 180, "Turkey"],
  ["RU", 520, 130, "Russia"],
  ["UA", 475, 155, "Ukraine"],
  ["IL", 485, 195, "Israel"],
  ["SA", 495, 215, "Saudi Arabia"],
  ["AE", 520, 215, "UAE"],
  ["IN", 590, 215, "India"],
  ["PK", 575, 205, "Pakistan"],
  ["BD", 615, 215, "Bangladesh"],
  ["CN", 650, 180, "China"],
  ["JP", 710, 185, "Japan"],
  ["KR", 690, 190, "South Korea"],
  ["TW", 685, 215, "Taiwan"],
  ["HK", 675, 220, "Hong Kong"],
  ["SG", 660, 255, "Singapore"],
  ["TH", 640, 235, "Thailand"],
  ["VN", 665, 230, "Vietnam"],
  ["ID", 680, 270, "Indonesia"],
  ["PH", 690, 240, "Philippines"],
  ["MY", 655, 260, "Malaysia"],
  ["AU", 715, 305, "Australia"],
  ["NZ", 755, 335, "New Zealand"],
  ["ZA", 475, 305, "South Africa"],
  ["EG", 475, 210, "Egypt"],
  ["NG", 430, 245, "Nigeria"],
  ["KE", 480, 265, "Kenya"],
  ["MA", 400, 200, "Morocco"],
];

// Simplified continent outlines — rough landmass shapes
const CONTINENTS_PATH = "M 50,130 Q 100,100 160,110 L 220,130 L 230,170 L 210,200 L 180,220 L 170,250 L 200,280 L 250,300 L 290,310 L 280,340 L 250,370 L 220,370 L 190,340 L 160,320 L 120,290 L 80,230 L 60,180 Z M 370,120 L 480,110 L 500,140 L 520,130 L 570,130 L 600,150 L 620,140 L 680,140 L 720,170 L 750,180 L 720,220 L 700,250 L 680,280 L 660,310 L 620,300 L 600,280 L 580,290 L 540,285 L 510,250 L 480,245 L 450,260 L 440,290 L 410,320 L 380,320 L 380,280 L 400,240 L 410,200 L 380,180 L 370,150 Z M 700,280 L 760,290 L 780,310 L 770,340 L 720,350 L 700,330 Z";

export default function WorldMap({ data }: Props) {
  const [hover, setHover] = useState<{ name: string; visitors: number; x: number; y: number } | null>(null);

  // Normalize visitor counts
  const max = Math.max(...data.map(d => d.visitors), 1);
  const byCode: Record<string, number> = {};
  for (const d of data) {
    const code = d.country.toUpperCase();
    byCode[code] = (byCode[code] || 0) + d.visitors;
  }

  const radiusFor = (visitors: number): number => {
    if (visitors === 0) return 0;
    const ratio = visitors / max;
    return Math.max(3, Math.min(20, Math.sqrt(ratio) * 20));
  };

  return (
    <div style={{ position: "relative", width: "100%" }}>
      <svg viewBox="0 0 800 400" style={{ width: "100%", display: "block", background: "var(--obs-surface)", borderRadius: "var(--obs-radius-md)" }}>
        {/* Continent landmass backgrounds */}
        <path d={CONTINENTS_PATH} fill="var(--obs-border-subtle)" stroke="var(--obs-border)" strokeWidth="0.5" opacity="0.5" />

        {/* Country dots */}
        {COUNTRY_POSITIONS.map(([code, x, y, name]) => {
          const visitors = byCode[code] || 0;
          const r = radiusFor(visitors);
          if (r === 0) {
            return <circle key={code} cx={x} cy={y} r="2" fill="var(--obs-text-muted)" opacity="0.3" />;
          }
          return (
            <circle key={code}
              cx={x} cy={y} r={r}
              fill="var(--obs-accent)"
              opacity="0.7"
              stroke="var(--obs-surface)"
              strokeWidth="1"
              style={{ cursor: "pointer" }}
              onMouseEnter={() => setHover({ name, visitors, x, y })}
              onMouseLeave={() => setHover(null)}
            />
          );
        })}
      </svg>

      {hover && (
        <div style={{
          position: "absolute",
          left: `${(hover.x / 800) * 100}%`,
          top: `${(hover.y / 400) * 100}%`,
          transform: "translate(-50%, calc(-100% - 12px))",
          background: "var(--obs-elevated)",
          border: "1px solid var(--obs-border)",
          borderRadius: "var(--obs-radius)",
          padding: "6px 10px",
          fontSize: "12px",
          color: "var(--obs-text)",
          whiteSpace: "nowrap",
          pointerEvents: "none",
          zIndex: 1,
          boxShadow: "0 2px 8px rgba(0,0,0,0.3)",
        }}>
          <div style={{ fontWeight: 600 }}>{hover.name}</div>
          <div style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums" }}>
            {hover.visitors.toLocaleString()} visitor{hover.visitors !== 1 ? "s" : ""}
          </div>
        </div>
      )}
    </div>
  );
}
