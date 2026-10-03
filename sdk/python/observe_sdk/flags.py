"""Feature-flag evaluation helper.

Wire contract (cmd/observe flagEvaluateHandler, internal/flags)::

    POST {endpoint}/api/v1/flags/evaluate
    {"site_id", "flag_key", "user_id", "context": {str: str}}
    -> {"enabled": bool, "variant"?: str, "reason": "evaluated" |
        "unavailable" | "invalid", "detail"?: str}

``evaluate_flag`` never raises. A network error, timeout, non-2xx status,
malformed body, or a server answer whose reason is not ``"evaluated"``
yields the caller-supplied ``default`` with ``source == "default"`` and the
cause in ``error``. There is no client-side cache (cache in your app if the
flag is read on a hot path); the server logs each enabled evaluation.
"""

from __future__ import annotations

import json
from typing import Any, Dict, Mapping, Optional
from urllib import request as urlrequest

_MAX_RESPONSE = 64 * 1024


def _fallback(key: str, default: Optional[Mapping[str, Any]], error: str) -> Dict[str, Any]:
    d = default or {}
    out: Dict[str, Any] = {
        "key": key,
        "enabled": d.get("enabled") is True,
        "reason": "default",
        "source": "default",
        "error": error,
    }
    if isinstance(d.get("variant"), str) and d["variant"]:
        out["variant"] = d["variant"]
    return out


def _attributes(attrs: Optional[Mapping[str, Any]]) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for k, v in (attrs or {}).items():
        if isinstance(v, str):
            out[str(k)] = v
        elif isinstance(v, (bool, int, float)):
            out[str(k)] = str(v).lower() if isinstance(v, bool) else str(v)
    return out


def evaluate(
    client: Any,
    key: str,
    *,
    user_id: Optional[str] = None,
    attributes: Optional[Mapping[str, Any]] = None,
    default: Optional[Mapping[str, Any]] = None,
    timeout: Optional[float] = None,
) -> Dict[str, Any]:
    """Evaluate ``key`` using ``client``'s endpoint, key, opener and site."""
    try:
        if not isinstance(key, str) or not key:
            return _fallback(str(key), default, "flag key required")
        ident = user_id
        if ident is None:
            ident = client._identity.get() or ""
        body = json.dumps({
            "site_id": client.opts.site_id,
            "flag_key": key,
            "user_id": ident,
            "context": _attributes(attributes),
        }).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        if client.opts.api_key:
            headers["X-API-Key"] = client.opts.api_key
        url = client.opts.endpoint.rstrip("/") + "/api/v1/flags/evaluate"
        req = urlrequest.Request(url, data=body, headers=headers, method="POST")
        t = timeout if isinstance(timeout, (int, float)) and not isinstance(timeout, bool) and timeout > 0 \
            else min(client.opts.timeout, 3.0)
        with client._opener.open(req, timeout=t) as resp:
            if not 200 <= resp.status < 300:
                return _fallback(key, default, f"status {resp.status}")
            raw = resp.read(_MAX_RESPONSE)
        parsed = json.loads(raw.decode("utf-8"))
        if not isinstance(parsed, dict) or not isinstance(parsed.get("enabled"), bool):
            return _fallback(key, default, "malformed response")
        reason = parsed.get("reason")
        if reason != "evaluated":
            # unavailable / invalid: the server answered its fail-safe, not a decision.
            return _fallback(key, default, f"server reason {reason or 'missing'}")
        out: Dict[str, Any] = {
            "key": key,
            "enabled": parsed["enabled"],
            "reason": reason,
            "source": "server",
        }
        if isinstance(parsed.get("variant"), str) and parsed["variant"]:
            out["variant"] = parsed["variant"]
        if isinstance(parsed.get("detail"), str) and parsed["detail"]:
            out["detail"] = parsed["detail"]
        return out
    except Exception as exc:  # noqa: BLE001 - documented: never raises
        return _fallback(key if isinstance(key, str) else str(key), default, f"{type(exc).__name__}: {exc}")
