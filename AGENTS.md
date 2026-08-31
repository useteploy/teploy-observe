# Working on teploy-observe

Instructions for coding agents. `CLAUDE.md` carries the detail on how this
codebase is built and tested; this file carries the one rule that is easy to get
wrong because it is about a repository other than this one.

## Which layer owns a defect

Observe runs on Neutron and stores in Nucleus, and **both are editable from
here — that is the trap.** `go.mod` replaces
`github.com/neutron-dev/neutron-go` with `../../Neutron/go`, which is the real
upstream working tree, so "fixing the framework" from an Observe session edits
the Neutron repository directly and silently. The `Neutron/` submodule in this
repo is a pinned reference copy and is **not** what compiles.

Neither is a place to land a framework fix from here.

**Fix a defect in the repository that owns it.**

- A wrong query, a bad migration, a scan bug, an ingest route — ours.
- A store that answers a correct query wrongly, or a framework that loses
  data — upstream.

Where you can only work around it from this side, the workaround ships **with a
logged report**, not instead of one, and the report records what the Observe-side
workaround was so the upstream fix can reconcile it.

**Never cut a Neutron or Nucleus release from a Teploy session.** An upstream fix
is a standalone change in the upstream repo plus a written handover.

In Tyler's own checkout the log is `Teploy/_internal/UPSTREAM_BUGS.md`
(append-only, template in the file; that directory is private and is not part of
this repository). Working from a clone without it, report upstream directly and
say in the commit message that you did.
