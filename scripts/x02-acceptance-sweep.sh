#!/usr/bin/env bash
# X02 S7 acceptance sweep — teploy-observe (ADR §6 S7).
# One executable harness running this repo's legs of the programme's
# acceptance line (:89) at the identity layer: duplicate delivery, identity
# under timestamp/version ties, repeated delivery, and history preservation
# under retention. Any leg failing (or matching no tests — vacuous pass is
# a broken pin) fails the sweep. Receipt: paste the output into AUDIT_OPEN
# when the contract changes.
set -u
cd "$(dirname "$0")/.."
fail=0
log=$(mktemp)

leg() {
  label="$1"; pkg="$2"; pattern="$3"
  printf '== %-24s %-30s ' "$label" "$pkg"
  listed=$(go test -list "$pattern" "$pkg" 2>/dev/null | grep -c '^Test')
  if [ "${listed:-0}" -eq 0 ]; then
    echo "FAIL (no tests matched: $pattern)"
    fail=1
    return
  fi
  if go test -count=1 "$pkg" -run "$pattern" >"$log" 2>&1; then
    echo "PASS ($listed test(s))"
  else
    echo "FAIL ($listed test(s), log: $log)"
    tail -5 "$log"
    fail=1
  fi
}

# identity under ties: same-millisecond writes stay strictly ordered —
# versions, tombstones, session references never resolve ambiguously.
leg identity-ties      ./internal/sso      'TestSameMillisecondCreateEnableYieldsStrictlyIncreasingVersions|TestEnableBumpsPastAFutureVersion'
leg identity-tombstone ./internal/platform 'TestSameMillisecondCreateDeleteTombstoneWins'
leg identity-sessions  ./internal/session  'TestReference_O04_TimestampTieBreak'

# duplicate delivery: v2 duplicate admission dedupes; same key with
# DIFFERENT content is not deduped (the repacking-producer hazard); v1 and
# malformed identities still admit.
leg duplicate-delivery ./internal/ingest  'TestBatchHandler_V2DuplicateAdmission|TestBatchHandler_SameKeyDifferentContentIsNotDeduped|TestBatchHandler_V1AndMalformedIdentityStillAdmit|TestPrepareEvent_ProducerEventID'

# history preservation: retention deletes the old, keeps the recent, prunes
# processed outbox intents but NEVER dead letters.
leg history-retention  ./internal/jobs    'TestRetentionDeletesOldKeepsRecent|TestDefaultLedgerPolicies|TestRetentionPrunesProcessedOutboxIntentsNotDeadLetters'

exit $fail
