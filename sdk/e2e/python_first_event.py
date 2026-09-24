"""O11 migration example fixture for the Python SDK: credential -> first
event -> flush(timeout) -> visible counters. Exercised end-to-end by
sdk/e2e/first_event_test.sh (env: OBSERVE_E2E_URL, OBSERVE_E2E_KEY)."""

import os
import sys
import pathlib

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "python"))

import observe_sdk  # noqa: E402

endpoint = os.environ.get("OBSERVE_E2E_URL")
api_key = os.environ.get("OBSERVE_E2E_KEY")
if not endpoint or not api_key:
    print("python-first-event: OBSERVE_E2E_URL and OBSERVE_E2E_KEY required")
    raise SystemExit(2)

client = observe_sdk.Client(
    endpoint=endpoint,
    api_key=api_key,
    site_id="default",
    service_name="o11-first-event",
    log_batch_size=100,
    log_flush_interval=3600.0,
)
client.info("o11 python first event", source="sdk-e2e")
client.flush(timeout=10.0)
client.close(timeout=10.0)

stats = client.stats()
if stats["delivered"] != 1 or stats["dropped"] or stats["queued"]:
    print(f"python-first-event: unexpected stats {stats}")
    raise SystemExit(1)
print("python-first-event: delivered=1 losses=0")
