// Ordering for the dashboard breakdown panels.
//
// The panels used to render the array the API returned as-is for the default
// (descending) direction and only sort on the ascending click. That relies on
// the engine returning a total order, and it does not: Nucleus emits GROUP BY
// results in hash order, so rows sharing a value come back in an order that
// changes with the LIMIT. On a small site most breakdown values tie at one or
// two visitors, which made the toggle look like it did nothing — the ascending
// pass is stable, so with every value equal it reproduced the same arbitrary
// order it was given.
//
// Sorting both directions here, with the label breaking ties, means the arrow
// always reverses what is on screen. The server applies the same tie-break in
// SQL so the top-N it truncates to is the same set this would keep.

export function sortRows<T extends Record<string, any>>(
  rows: T[],
  valueKey: string,
  labelKey: string,
  ascending: boolean,
): T[] {
  const direction = ascending ? 1 : -1;
  return [...rows].sort((a, b) => {
    const av = Number(a[valueKey]) || 0;
    const bv = Number(b[valueKey]) || 0;
    if (av !== bv) return (av - bv) * direction;
    return String(a[labelKey] ?? "").localeCompare(String(b[labelKey] ?? ""));
  });
}

/** Human name for the column a panel ranks on, for the sort button's label. */
export function metricLabel(valueKey: string): string {
  switch (valueKey) {
    case "pageviews":
      return "Pageviews";
    case "visitors":
      return "Visitors";
    case "count":
      return "Count";
    default:
      return valueKey.charAt(0).toUpperCase() + valueKey.slice(1);
  }
}
