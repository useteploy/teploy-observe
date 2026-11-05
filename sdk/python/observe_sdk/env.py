"""Environment fallbacks for options the caller left unset.

An explicit keyword always wins, then ``OBSERVE_*``, then the OpenTelemetry
standard names.

The credential follows the endpoint it was issued alongside: ``OBSERVE_API_KEY``
is only sent to an explicit or ``OBSERVE_ENDPOINT`` endpoint, and the
``x-api-key`` from ``OTEL_EXPORTER_OTLP_HEADERS`` only to
``OTEL_EXPORTER_OTLP_ENDPOINT``. An app whose ``OTEL_*`` point at another vendor's
collector must not have its Observe key sent there, nor that vendor's key sent to
Observe.

``OTEL_EXPORTER_OTLP_PROTOCOL=grpc`` disables the OTLP endpoint and headers: this
client speaks HTTP and that endpoint would be a gRPC port. ``http/protobuf`` and
``http/json`` are fine because Observe accepts both encodings and the endpoint is
the same HTTP base.
"""

import os
from typing import Any, Dict
from urllib.parse import unquote


def _parse_otel_list(raw: str, lower_keys: bool = False) -> Dict[str, str]:
    """Read the comma-separated key=value list OTEL env vars use.

    Values are percent-encoded. A malformed entry is skipped, as the OTel spec
    asks. Header names are case-insensitive, so ``lower_keys`` folds them.
    """
    out: Dict[str, str] = {}
    for entry in raw.split(","):
        key, sep, value = entry.partition("=")
        key = key.strip()
        if not sep or not key:
            continue
        out[key.lower() if lower_keys else key] = unquote(value.strip())
    return out


def apply_env(kwargs: Dict[str, Any]) -> Dict[str, Any]:
    """Return a copy of ``kwargs`` with unset options filled from the environment."""
    merged = dict(kwargs)
    env = os.environ

    if not merged.get("endpoint"):
        merged["endpoint"] = env.get("OBSERVE_ENDPOINT", "")
    if merged["endpoint"]:
        if not merged.get("api_key"):
            merged["api_key"] = env.get("OBSERVE_API_KEY") or None
    elif env.get("OTEL_EXPORTER_OTLP_PROTOCOL", "").strip() in ("", "http/protobuf", "http/json"):
        merged["endpoint"] = env.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
        if not merged.get("api_key"):
            headers = _parse_otel_list(env.get("OTEL_EXPORTER_OTLP_HEADERS", ""), lower_keys=True)
            merged["api_key"] = headers.get("x-api-key") or None

    attrs = _parse_otel_list(env.get("OTEL_RESOURCE_ATTRIBUTES", ""))
    fallbacks = {
        "service_name": env.get("OTEL_SERVICE_NAME") or attrs.get("service.name"),
        "release": attrs.get("service.version"),
        "environment": attrs.get("deployment.environment.name") or attrs.get("deployment.environment"),
    }
    for option, value in fallbacks.items():
        if not merged.get(option) and value:
            merged[option] = value
    return merged
