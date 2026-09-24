"""O11 adverse-transport tests for the Python SDK.

Fault modes mirror sdk/testing/README.md (shared matrix): transient 500s,
permanent 500s into a visible retry-exhausted loss, non-retryable 4xx
poison heads, partial per-record acks, slow responses against shutdown
deadlines, and the loss-accounting identity.
"""

from __future__ import annotations

import time

import pytest

from faultserver import FaultServer

import observe_sdk


@pytest.fixture()
def fault():
    server = FaultServer()
    yield server
    server.close()


def _drain(client, rounds=60):
    """Flush until the queue empties or the loss counters settle."""
    for _ in range(rounds):
        if client.stats()["queued"] == 0:
            return
        try:
            client.flush()
        except Exception:
            pass
        time.sleep(0.02)


def test_transient_500s_recover_with_zero_loss(fault):
    fault.set_mode({"kind": "failN", "n": 2})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        max_send_attempts=6,
        retry_backoff=0.001,
    )
    client.info("one")
    client.info("two")
    _drain(client)

    stats = client.stats()
    assert stats["delivered"] == 2, stats
    assert stats["retries"] >= 2, stats
    assert stats["dropped"] == {}, stats
    assert stats["queued"] == 0
    client.close()


def test_permanent_500s_exhaust_into_visible_loss(fault):
    fault.set_mode({"kind": "status", "code": 500})
    errors = []
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        max_send_attempts=3,
        retry_backoff=0.001,
    )
    client.on_error = lambda exc: errors.append(str(exc))
    client.info("doomed")
    for _ in range(60):
        try:
            client.flush()
        except Exception:
            pass
        if client.stats()["dropped"].get("retry_exhausted"):
            break
        time.sleep(0.02)

    stats = client.stats()
    assert stats["dropped"].get("retry_exhausted") == 1, stats
    assert stats["queued"] == 0

    # The dropped chunk must not poison the client: new entries deliver.
    fault.set_mode({"kind": "ok"})
    client.info("after-recovery")
    _drain(client)
    assert client.stats()["delivered"] == 1
    assert any("retry_exhausted" in e for e in errors)
    client.close()


def test_non_retryable_4xx_drops_poison_head_and_drains(fault):
    fault.set_mode({"kind": "status", "code": 400})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        max_send_attempts=6,
        retry_backoff=0.001,
    )
    client.info("poison")
    client.flush()  # 400: dropped immediately, no retry budget burned
    stats = client.stats()
    assert stats["dropped"].get("non_retryable") == 1, stats
    assert stats["queued"] == 0

    fault.set_mode({"kind": "ok"})
    client.info("survivor")
    client.flush()
    stats = client.stats()
    assert stats["delivered"] == 1, stats
    assert stats["dropped"].get("non_retryable") == 1, stats
    client.close()


def test_partial_rejection_counts_per_record_outcomes(fault):
    fault.set_mode({"kind": "partial", "ack": {"accepted": 1, "rejected": 1}})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
    )
    client.info("good")
    client.info("bad")
    client.flush()
    time.sleep(0.05)  # a resend would appear as a second POST

    stats = client.stats()
    assert fault.posts_for("/api/v1/logs/batch") == 1, "batch must not be resent"
    assert stats["dropped"].get("server_rejected") == 1, stats
    assert stats["delivered"] == 1, stats
    client.close()


def test_close_deadline_counts_unflushed_as_losses(fault):
    fault.set_mode({"kind": "slow", "delay": 5.0})
    errors = []
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        timeout=10.0,
    )
    client.on_error = lambda exc: errors.append(str(exc))
    client.info("stuck-1")
    client.info("stuck-2")

    with pytest.raises(TimeoutError):
        client.close(timeout=0.3)

    stats = client.stats()
    assert stats["dropped"].get("shutdown_unflushed") == 2, stats
    assert any("shutdown_unflushed" in e for e in errors)


def test_flush_deadline_leaves_queue_visible(fault):
    fault.set_mode({"kind": "slow", "delay": 5.0})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        timeout=10.0,
    )
    client.info("stuck")
    # The per-request timeout fires before the whole-drain deadline and
    # surfaces as a retryable RuntimeError; either way the entry must
    # remain queued and visible, never swallowed.
    with pytest.raises((TimeoutError, RuntimeError)):
        client.flush(timeout=0.3)
    stats = client.stats()
    assert stats["queued"] == 1, "the timed-out entry must remain visible"

    fault.set_mode({"kind": "ok"})
    client.flush()
    assert client.stats()["queued"] == 0
    client.close()


def test_accounting_identity_delivered_plus_dropped(fault):
    fault.set_mode({"kind": "ok"})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
    )
    for i in range(10):
        client.info(f"entry-{i}")
    _drain(client)
    client.close(timeout=10.0)

    stats = client.stats()
    lost = sum(stats["dropped"].values())
    assert stats["delivered"] + lost + stats["queued"] == 10, stats


def test_connection_reset_follows_retry_path(fault):
    fault.set_mode({"kind": "reset"})
    client = observe_sdk.Client(
        endpoint=fault.url,
        log_batch_size=100,
        log_flush_interval=3600.0,
        max_send_attempts=6,
        retry_backoff=0.001,
    )
    client.info("reset-me")
    with pytest.raises(Exception):
        client.flush()
    assert fault.posts_for("/api/v1/logs/batch") >= 1

    fault.set_mode({"kind": "ok"})
    _drain(client)
    stats = client.stats()
    assert stats["delivered"] == 1, stats
    assert stats["retries"] >= 1, stats
    assert stats["dropped"] == {}, stats
    client.close()
