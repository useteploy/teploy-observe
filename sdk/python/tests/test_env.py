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
    got = apply_env(explicit)
    for option, value in explicit.items():
        assert got[option] == value


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
    assert not got["endpoint"] if "endpoint" in got else True
    assert not got.get("api_key")


@pytest.mark.parametrize(
    "observe_endpoint,want_key",
    [
        (None, "obs_key"),
        ("https://code.example.com/", "obs_key"),
        ("https://other.example.com", None),
    ],
)
def test_explicit_endpoint_and_env_key(monkeypatch, observe_endpoint, want_key):
    monkeypatch.setenv("OBSERVE_API_KEY", "obs_key")
    if observe_endpoint:
        monkeypatch.setenv("OBSERVE_ENDPOINT", observe_endpoint)
    got = apply_env({"endpoint": "https://code.example.com"})
    assert got["endpoint"] == "https://code.example.com"
    assert got.get("api_key") == want_key


def test_vendor_otel_endpoint_with_no_header_is_an_error(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    with pytest.raises(ValueError):
        Client()


def test_observe_key_does_not_qualify_an_otel_endpoint(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    monkeypatch.setenv("OBSERVE_API_KEY", "obs_key")
    with pytest.raises(ValueError):
        Client()


def test_explicit_key_does_not_qualify_an_otel_endpoint(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
    with pytest.raises(ValueError):
        Client(api_key="obs_code")


def test_observe_endpoint_wins_and_takes_no_vendor_key(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://other-vendor.example.com")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
    monkeypatch.setenv("OBSERVE_ENDPOINT", "https://ingest.example.com")
    got = apply_env({})
    assert got["endpoint"] == "https://ingest.example.com"
    assert not got.get("api_key")


def test_explicit_endpoint_takes_no_vendor_key(monkeypatch):
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=vendor_key")
    assert not apply_env({"endpoint": "https://code.example.com"}).get("api_key")


def test_site_id_defaults_only_without_a_key():
    assert apply_env({"endpoint": "http://x"})["site_id"] == "default"
    assert apply_env({"endpoint": "http://x", "api_key": "k"})["site_id"] == ""
    assert apply_env({"endpoint": "http://x", "api_key": "k", "site_id": "mine"})["site_id"] == "mine"


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
