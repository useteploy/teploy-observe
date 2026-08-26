# Map geometry

`world110m.ts` is the country geometry behind the "Visitors by Country"
choropleth. It is generated, committed, and imported by the bundle — the
dashboard ships self-hosted and often air-gapped, so nothing fetches a map at
build or run time.

## Source

Natural Earth, 1:110m Cultural Vectors:

- **Admin 0 – Countries** — the polygons.
- **Admin 0 – Tiny Country Points** — the point locations Natural Earth
  publishes for countries too small to have a polygon at this scale (Singapore,
  Malta, Bahrain, Maldives and so on).

Retrieved from the `nvkelso/natural-earth-vector` distribution repository at tag
`v5.1.2`, which is the maintainer's own mirror of the naturalearthdata.com
releases:

- `https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_110m_admin_0_countries.geojson`
- `https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_110m_admin_0_tiny_countries.geojson`

## Licence

Public domain. Natural Earth's terms of use:

> All versions of Natural Earth raster and vector map data found on this website
> are in the public domain. You may use the maps in any manner, including
> modifying the content and design, electronic dissemination, and offset
> printing. The primary authors, Tom Patterson and Nathaniel Vaughn Kelso, and
> all other contributors renounce all financial claim to the underlying data and
> iterations of the maps.

No attribution is required and no notice has to travel with the data. The
provenance is recorded here and in the generated file's header because knowing
where geometry came from matters when it needs refreshing, not because a licence
demands it.

## What the generator does to it

`scripts/gen-world-geometry.mjs`:

- drops Antarctica and clips latitude to `[-59, 84]` — no analytics value, and
  Antarctica alone is the largest single contributor of vertices;
- simplifies each ring with Douglas-Peucker at 0.12 degrees (about 0.27 px at a
  1000 px-wide panel, so the loss is sub-pixel);
- rounds coordinates to two decimal places and drops the duplicates that
  creates;
- drops rings spanning under 0.45 degrees unless a ring is the last thing
  keeping its country on the map;
- resolves each feature's ISO-3166-1 alpha-2 code from `ISO_A2_EH`, falling back
  to `ISO_A2`. `ISO_A2_EH` is what makes France and Norway resolve — both carry
  `-99` in the plain `ISO_A2` field. The two features with no ISO code at all
  (Somaliland, Northern Cyprus) are emitted separately as neutral land that can
  never take a fill.

Path coordinates are degrees written as `longitude,-latitude`, so
`WORLD_VIEWBOX` **is** the equirectangular (plate carrée) projection and the
renderer does no projection maths.

## Regenerating

```
node scripts/gen-world-geometry.mjs
```

Needs network access once, to fetch the two source files (cached under
`$NE_CACHE_DIR`, default `$TMPDIR/ne-110m-cache`). `NE_TOLERANCE` overrides the
simplification tolerance. Re-run `ui/` tests afterwards: they assert the code
mapping and the viewBox against whatever the generator produced.

Natural Earth does not carry Hong Kong or Macao as admin-0 features at any
scale, so `HK` and `MO` have no geometry to match. That is expected — the panel
counts unmatched codes and prints them under the map rather than dropping them.
