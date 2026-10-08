# Release assurance

Product tags call the reusable CI workflow at the tag's exact commit. Publication
requires every declared gate to succeed, including the canonical embedded UI
rebuild, immutable Neutron/vendor parity, SDK tests, race checks, live Nucleus on
both architectures, snapshot leases, browser journeys, and replay regeneration.
The final receipt binds the source bytes, commit, Actions run and attempt. Release
verification refuses receipts from another commit, run or attempt and refuses
missing, failed or skipped gates. A prior main-branch green result does not satisfy
this requirement.

`bash scripts/regenerate-replay.sh --check` builds the recorder and both player
bundles twice in disposable directories using the committed npm lock. Both builds
must agree with each other and with the committed assets. Under a controlled build
slot, use `--update` to refresh those three assets, review them, then run the check
and canonical `scripts/ui-sync.sh` pipeline. UI freshness refuses tracked or new
embedded output drift. The upstream checkout and workspace lock patch must be
updated together when integrating an approved Neutron pin.

The Helm pair gate renders the chart's actual defaults, verifies published
amd64/arm64 manifests, records pulled digests, boots those images together, runs
migrations and SQL-backed health, and exercises the seeded browser journeys.
Historical default versions are retained until an approved published replacement
pair passes acceptance. A source candidate or successful source-engine suite is
not evidence that its image is published. This gate can remain red while that
external acceptance is pending; it must not be bypassed by inventing a version.
The gate checks the default container pair; Kubernetes CNI, persistent-volume,
upgrade and cluster acceptance remain separate deployment checks.

SDK tags must match the package version. Node jobs use `npm ci` and run tests
before packaging/publication. Python 3.12 uses the fully version-resolved
`requirements-ci.lock` with dependency resolution and build isolation disabled;
it runs tests before building. This freezes the test/build dependency versions,
not registry availability or artifact hashes. The PyPI publication Action and
other version-tagged Actions remain explicit tool provenance limits.

These workflows define future gates. Source editing or parser checks alone do not
establish a successful hosted run, published artifact, deployed adoption, browser
acceptance, or approved upstream integration.
