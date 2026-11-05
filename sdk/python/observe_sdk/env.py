"""Environment fallbacks for options the caller left unset.

An explicit keyword always wins, then ``OBSERVE_*``, then the OpenTelemetry
standard names.

A credential only goes where it was issued for:

- An explicit ``endpoint`` takes ``OBSERVE_API_KEY`` only if ``OBSERVE_ENDPOINT`` is
  unset or names the same place. Otherwise that key belongs to another instance.
- ``OTEL_EXPORTER_OTLP_ENDPOINT`` is used only when ``OTEL_EXPORTER_OTLP_HEADERS``
  carries ``x-api-key`` and no key was passed in code. Without that header the
  endpoint is some other vendor's collector, so the client keeps refusing to start
  rather than send telemetry there. With a key in code, using it would send that
  key there.

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


def _same_endpoint(a: str, b: str) -> bool:
    return a.rstrip("/") == b.rstrip("/")


def apply_env(kwargs: Dict[str, Any]) -> Dict[str, Any]:
    """Return a copy of ``kwargs`` with unset options filled from the environment."""
    merged = dict(kwargs)
    env = os.environ

    observe_endpoint = env.get("OBSERVE_ENDPOINT", "")
    if merged.get("endpoint"):
        same = _same_endpoint(observe_endpoint, merged["endpoint"])
        if not merged.get("api_key") and (not observe_endpoint or same):
            merged["api_key"] = env.get("OBSERVE_API_KEY") or None
    elif observe_endpoint:
        merged["endpoint"] = observe_endpoint
        if not merged.get("api_key"):
            merged["api_key"] = env.get("OBSERVE_API_KEY") or None
    elif not merged.get("api_key") and env.get("OTEL_EXPORTER_OTLP_PROTOCOL", "").strip() in (
        "",
        "http/protobuf",
        "http/json",
    ):
        headers = _parse_otel_list(env.get("OTEL_EXPORTER_OTLP_HEADERS", ""), lower_keys=True)
        if headers.get("x-api-key"):
            merged["endpoint"] = env.get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
            merged["api_key"] = headers["x-api-key"]

    attrs = _parse_otel_list(env.get("OTEL_RESOURCE_ATTRIBUTES", ""))
    fallbacks = {
        "service_name": env.get("OTEL_SERVICE_NAME") or attrs.get("service.name"),
        "release": attrs.get("service.version"),
        "environment": attrs.get("deployment.environment.name") or attrs.get("deployment.environment"),
    }
    for option, value in fallbacks.items():
        if not merged.get(option) and value:
            merged[option] = value

    # A key is bound to one site on the server, and a site_id that disagrees with
    # it is refused with a 403. So with a key, send none and let the key decide;
    # "default" is only for keyless use.
    if not merged.get("site_id"):
        merged["site_id"] = "" if merged.get("api_key") else "default"
    return merged
