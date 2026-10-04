# Experiments: analysis contract

## Fixed horizon, not sequential

Results are a **fixed-horizon** analysis (alpha 0.05 two-sided). Nothing here is
sequential-safe: looking at p-values repeatedly and stopping when one dips below
0.05 inflates the false-positive rate. The contract:

1. Plan the sample size first (`GET /api/v1/experiments/sample-size?p1=&mde=`).
   Record it with `PUT /api/v1/experiments/{id}/settings`
   (`planned_sample_per_arm`).
2. Until **every** arm has `max(min_sample, planned_sample_per_arm)` exposed
   users, results carry `peeking_warning` and the winner gate stays shut
   (`analysis.horizon_met = false`, `winner = ""`, `significant = false`).
   Estimates and intervals are still shown as a progress report.
3. Override: `?allow_early=true` on the results request, or
   `allow_early_winner: true` in the settings. This waives only the
   planned-sample part; the `min_sample` floor (and 30 per arm for count/mean
   metrics) is never waived. The output records `horizon_overridden`.

## Who is counted

A user exposed to more than one variant inside the running interval is
**contaminated**: excluded from every count and statistic and reported in
`contaminated_users`. A rising count means assignment is leaking between arms;
investigate before trusting the result.

## Metric kinds

Settings (`PUT /api/v1/experiments/{id}/settings`, body also carries `site_id`):

| field | meaning |
|---|---|
| `metric_kind` | `binary` (default), `count`, `mean` |
| `winsorize_pct` | 0 = off; else clip count/mean values to the pooled `[p, 100-p]` percentiles (linear interpolation, numpy default) |
| `planned_sample_per_arm` | design-time n, see above |
| `allow_early_winner` | persistent override of the planned-sample gate |
| `secondary_goals` | up to 3 of `{key, name, kind}`; `key` matches `^[a-z0-9_]{1,32}$` |

- **binary**: conversions come from `/experiments/convert`; chi-square / Fisher /
  Holm / Wilson / Newcombe as before, Bayesian probability-to-beat shown.
- **count / mean**: observations are posted to `POST /api/v1/experiments/metric`
  (`{experiment_id, user_id, metric, value}`; `metric` is `primary` or a
  secondary key; API-key auth, ignored for users with no exposure). The per-user
  value is over events inside the conversion window after the user's first
  exposure: the number of events (count) or the sum of `value` (mean, e.g.
  revenue per exposed user; non-buyers contribute 0). Each arm is compared with
  the control by Welch's t-test (Welch-Satterthwaite df), 95% CI of the mean
  difference, Holm across arms. Higher is better. No Bayesian value is shown.

## Secondary metrics

Evaluated on the same clean cohort with the same machinery, labelled
`role: "secondary"`, `gates_winner: false`. They never produce a winner.
Holm applies across the arms of one metric, not across metrics, so a secondary
crossing 0.05 is exploratory (`multiple_comparison_note`).

## Bayesian display value

`prob_beat_control` is a Monte Carlo estimate seeded from the experiment id and a
hash of the arm counts: stable between identical calls, independent across
experiments. It is display-only and never gates the winner.

## Not covered

Auto-recording an exposure from `/flags/evaluate` is not implemented (it needs a
hook in `internal/flags`); exposures are still posted to `/experiments/expose`.
