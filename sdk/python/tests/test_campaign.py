import json
import math
import threading
import time
from unittest.mock import patch

import pytest
import observe_sdk
import observe_sdk.client as module
from observe_sdk.breadcrumbs import BreadcrumbBuffer


def test_package_timeout_and_stats_api_and_invalid_replacement():
    old = observe_sdk.init(endpoint="http://localhost:1", log_flush_interval=3600)
    try:
        observe_sdk.flush(timeout=0.1)
        assert observe_sdk.stats()["queued"] == 0
        with pytest.raises(ValueError):
            observe_sdk.init(endpoint="")
        assert module._default is old
        assert old._state == "open"
        observe_sdk.close(timeout=0.1)
    finally:
        old.close()
        module._default = None


def test_nested_breadcrumb_snapshots_and_nonfinite_data():
    buffer = BreadcrumbBuffer()
    buffer.add("nested", data={"nested": {"list": [1]}})
    first = buffer.snapshot()
    first[0]["data"]["nested"]["list"][0] = 9
    assert buffer.snapshot()[0]["data"]["nested"]["list"] == [1]
    buffer.add("nan", data={"nested": [math.nan, math.inf]})
    json.dumps(buffer.snapshot(), allow_nan=False)
    assert buffer.snapshot()[-1]["data"] == {"_unserializable": True}


def test_flush_and_close_ownership_waits_share_deadline():
    client = observe_sdk.Client(endpoint="http://localhost:1", log_flush_interval=3600)
    client._flush_lock.acquire()
    try:
        for action in (client.flush, client.close):
            started = time.monotonic()
            with pytest.raises(TimeoutError):
                action(timeout=0.02)
            assert time.monotonic() - started < 0.15
    finally:
        client._flush_lock.release()
        client.close()


def test_retryable_close_does_not_count_retained_records_as_lost():
    client = observe_sdk.Client(endpoint="http://localhost:1", log_flush_interval=3600)
    client.info("recoverable")
    try:
        with patch.object(client, "_post_batch", side_effect=OSError("offline")):
            for _ in range(2):
                with pytest.raises(TimeoutError):
                    client.close(timeout=0.1)
                assert client.stats()["queued"] == 1
                assert client.stats()["dropped"] == {}
        with patch.object(client, "_post_batch", return_value=(1, 0)):
            client.close(timeout=0.1)
        stats = client.stats()
        assert stats["delivered"] == 1
        assert stats["queued"] == 0
        assert stats["dropped"] == {}
    finally:
        with patch.object(client, "_post_batch", return_value=(1, 0)):
            client.close()


def test_strict_error_wire_and_aggregate_breadcrumb_budget():
    client = observe_sdk.Client(endpoint="http://localhost:1", log_flush_interval=3600)
    bodies = []

    def strict_wire(path, data, **kwargs):
        def reject_nonfinite(value):
            raise ValueError(value)
        bodies.append(json.loads(data, parse_constant=reject_nonfinite))
        assert len(data) < 256 * 1024

    try:
        client.add_breadcrumb("nonfinite", data={"nested": [math.nan, math.inf, -math.inf]})
        with patch.object(client, "_post_bytes", side_effect=strict_wire):
            client.capture_exception(ValueError("first"))
            client.capture_exception(ValueError("second"))
            for i in range(100):
                client.add_breadcrumb(str(i) + "x" * 256, data={"k": "x" * 1000})
            client.capture_exception(ValueError("aggregate"))
        assert len(bodies) == 3
        assert bodies[0]["breadcrumbs"][0]["data"] == {"_unserializable": True}
        assert 0 < len(bodies[-1]["breadcrumbs"]) < 100
    finally:
        client.close()
