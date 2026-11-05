import pytest

from observe_sdk import Client
from observe_sdk.env import _parse_otel_list, apply_env

ENV_VARS = [
    "OBSERVE_ENDPOINT",
    "OBSERVE_API_KEY",
    "OTEL_EXPORTER_OTLP_ENDPOINT",
    "OTEL_EXPORTER_OTLP_PROTOCOL",
    "OTEL_EXPORTER_OTLP_HEADERS",
    "OTEL_SERVICE_NAME",
    "OTEL_RESOURCE_ATTRIBUTES",
]


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for name in ENV_VARS:
        monkeypatch.delenv(name, raising=False)


def test_observe_names(monkeypatch):
    monkeypatch.setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
    monkeypatch.setenv("OBSERVE_API_KEY", "obs_key")
    got = apply_env({})
    assert got["endpoint"] == "https://ingest.example.com"
    assert got["api_key"] == "obs_key"


def test_explicit_options_win(monkeypatch):
    monkeypatch.setenv("OBSERVE_ENDPOINT", "https://env.example.com")
    monkeypatch.setenv("OBSERVE_API_KEY", "obs_env")
    monkeypatch.setenv("OTEL_SERVICE_NAME", "env-name")
    monkeypatch.setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=9,deployment.environment=env")
    explicit = {
        "endpoint": "https://code.example.com",
        "api_key": "obs_code",
        "service_name": "code",
        "release": "1",
        "environment": "code",
    }
    assert apply_env(explicit) == explicit


def test_otel_contract(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://ingest.example.com")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "other=1, X-Api-Key=obs%5Fkey")
    monkeypatch.setenv("OTEL_SERVICE_NAME", "shop")
    monkeypatch.setenv(
        "OTEL_RESOURCE_ATTRIBUTES",
        "service.name=ignored,service.version=1.2.3,deployment.environment.name=prod",
    )
    got = apply_env({})
    assert got["endpoint"] == "https://ingest.example.com"
    assert got["api_key"] == "obs_key"
    assert got["service_name"] == "shop"
    assert got["release"] == "1.2.3"
    assert got["environment"] == "prod"


def test_otel_grpc_is_not_usable(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.com:4317")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=obs_key")
    got = apply_env({})
    assert not got["endpoint"]
    assert not got.get("api_key")


def test_vendor_key_never_reaches_observe(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
    monkeypatch.setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
    got = apply_env({})
    assert got["endpoint"] == "https://ingest.example.com"
    assert not got.get("api_key")


def test_observe_key_never_reaches_another_vendor(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    monkeypatch.setenv("OBSERVE_API_KEY", "obs_key")
    got = apply_env({})
    assert got["endpoint"] == "https://other-vendor.example.com"
    assert not got.get("api_key")


def test_vendor_key_never_reaches_explicit_endpoint(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
    assert not apply_env({"endpoint": "https://code.example.com"}).get("api_key")


def test_client_reads_env_and_still_requires_an_endpoint(monkeypatch):
    with pytest.raises(ValueError):
        Client()
    monkeypatch.setenv("OBSERVE_ENDPOINT", "http://127.0.0.1:1")
    client = Client()
    try:
        assert client.opts.endpoint == "http://127.0.0.1:1"
    finally:
        client.close()


def test_parse_skips_malformed():
    assert _parse_otel_list("a=1,broken,=2,c=x%3Dy") == {"a": "1", "c": "x=y"}
