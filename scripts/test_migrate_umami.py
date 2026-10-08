#!/usr/bin/env python3
"""Offline importer contracts: execute the POSIX script with fake psql/curl.

Run: python3 scripts/test_migrate_umami.py
Optional: UMAMI_TEST_SHELL=/bin/dash to exercise another POSIX shell.
No source database, Observe server, or Nucleus connection is made.
"""
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("migrate-umami.sh").resolve()
FIRST = "11111111-1111-1111-1111-111111111111"
SECOND = "22222222-2222-2222-2222-222222222222"
SESSION = "33333333-3333-3333-3333-333333333333"
WEBSITE = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
SECRET = "fixture-key-do-not-log"

PSQL = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
sql = sys.stdin.read()
assert '-X' in args and 'ON_ERROR_STOP=1' in args
assert os.environ['PGDATABASE'] == 'postgres://fixture:source-secret@unused/umami'
assert not any('source-secret' in x for x in args)
assert "e.event_id > :'after_id'::uuid" in sql
assert "e.website_id = NULLIF(:'website_id', '')::uuid" in sql
assert 'LEFT JOIN session s' in sql and 's.website_id = e.website_id' in sql
assert 'jsonb_build_object' in sql and 'ORDER BY e.event_id ASC' in sql
values = dict(x.split('=', 1) for x in args if '=' in x)
pathlib.Path(os.environ['FIXTURE_DIR'], 'psql-called').touch()
if os.environ.get('SOURCE_FAIL'):
    print('source-secret fixture-key-do-not-log', file=sys.stderr)
    sys.exit(1)
rows = json.loads(pathlib.Path(os.environ['FIXTURE_DIR'], 'source.json').read_text())
rows = [r for r in rows if r['event_id'].lower() > values['after_id'].lower()]
for row in rows[:int(values['batch'])]:
    print(json.dumps(row, ensure_ascii=False))
'''
CURL = r'''
import json, os, pathlib, stat, sys
args = sys.argv[1:]
assert args[0] == '--disable' and '--location' not in args
assert args[args.index('--request') + 1] == 'POST'
assert 'https://observe.invalid/api/v1/events/import' in args
assert not any('fixture-key-do-not-log' in x for x in args)
headers = pathlib.Path(args[args.index('--header') + 1][1:])
assert headers.read_text().splitlines() == ['X-API-Key: fixture-key-do-not-log', 'Content-Type: application/json']
assert stat.S_IMODE(headers.stat().st_mode) == 0o600
assert stat.S_IMODE(headers.parent.stat().st_mode) == 0o700
payload = pathlib.Path(args[args.index('--data-binary') + 1][1:])
data = json.loads(payload.read_text())
fixture = pathlib.Path(os.environ['FIXTURE_DIR'])
with (fixture / 'sent.jsonl').open('a') as log:
    log.write(json.dumps(data) + '\n')
if os.environ.get('TRANSPORT_FAIL'):
    print('fixture-key-do-not-log source-secret', file=sys.stderr)
    sys.exit(7)
response = os.environ.get('RESPONSE', json.dumps({'ok': True, 'accepted': len(data['events'])}))
pathlib.Path(args[args.index('--output') + 1]).write_text(response)
print(os.environ.get('HTTP', '200'), end='')
'''


class ImporterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="umami-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        bindir = self.root / "bin"
        bindir.mkdir()
        for name, body in [("psql", PSQL), ("curl", CURL)]:
            command = bindir / name
            command.write_text(f"#!{sys.executable}\n" + body)
            command.chmod(0o700)
        self.state = self.root / "checkpoint"
        self.env = {**os.environ, "PATH": str(bindir) + os.pathsep + os.environ["PATH"],
                    "UMAMI_DB": "postgres://fixture:source-secret@unused/umami",
                    "OBSERVE_ENDPOINT": "https://observe.invalid/", "OBSERVE_API_KEY": SECRET,
                    "OBSERVE_SITE_ID": "site", "UMAMI_WEBSITE_ID": WEBSITE,
                    "STATE_FILE": str(self.state), "TMPDIR": str(self.root),
                    "FIXTURE_DIR": str(self.root), "BATCH": "100", "STOP_AFTER": "0"}
        for name in ["SOURCE_FAIL", "TRANSPORT_FAIL", "RESPONSE", "HTTP"]:
            self.env.pop(name, None)
        self.rows = [self.row(FIRST), self.row(SECOND)]

    def row(self, identity):
        return {"event_id": identity, "session_id": SESSION, "timestamp": 1700000000123,
                "event_type": "pageview", "pathname": '/tab\tline\n"雪"', "url": '/tab?utm_source=old',
                "browser": "", "os": None, "country": "CA", "referrer": ""}

    def run_script(self, **env):
        (self.root / "source.json").write_text(json.dumps(self.rows))
        result = subprocess.run([os.environ.get("UMAMI_TEST_SHELL", "/bin/sh"), str(SCRIPT)], env={**self.env, **env},
                                capture_output=True, text=True, timeout=15)
        self.assertNotIn(SECRET, result.stdout + result.stderr)
        self.assertNotIn("source-secret", result.stdout + result.stderr)
        self.assertFalse(list(self.root.glob("observe-umami.*")), "private work files were not cleaned")
        self.assertFalse(list(self.root.glob("checkpoint.tmp.*")))
        self.assertFalse((self.root / "checkpoint.lock").exists())
        return result

    def sent(self):
        path = self.root / "sent.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_preserves_identity_time_session_path_and_resumes(self):
        result = self.run_script(BATCH="1", STOP_AFTER="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.sent()[0]["events"], [{**self.rows[0], "site_id": "site"}])
        checkpoint = json.loads(self.state.read_text())
        self.assertEqual(checkpoint["event_id"], FIRST)
        self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), 0o600)
        result = self.run_script(BATCH="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([e["events"][0]["event_id"] for e in self.sent()], [FIRST, SECOND])
        self.assertEqual(json.loads(self.state.read_text())["event_id"], SECOND)

    def test_stop_after_limits_first_batch(self):
        self.assertEqual(self.run_script(STOP_AFTER="1").returncode, 0)
        self.assertEqual(len(self.sent()[0]["events"]), 1)
        self.assertEqual(json.loads(self.state.read_text())["event_id"], FIRST)

    def test_refusals_never_advance_existing_checkpoint(self):
        cases = [{"HTTP": code} for code in ["000", "199", "301", "400", "401", "403", "429", "503"]]
        cases += [{"TRANSPORT_FAIL": "1"}, {"SOURCE_FAIL": "1"}]
        cases += [{"RESPONSE": value} for value in [
            '{"ok":true,"accepted":0}', '{"ok":false,"accepted":1}',
            '{"ok":true,"accepted":1,"rejected":1}', '{"ok":true,"accepted":"1"}',
            '{"ok":true,"accepted":1,"rejected":null}', '{"ok":true}',
            'not json fixture-key-do-not-log', '[]',
            '{"ok":true,"accepted":1}\n{"ok":true,"accepted":1}']]
        for env in cases:
            with self.subTest(env=env):
                self.state.write_text(FIRST + "\n")
                before = self.state.read_bytes()
                result = self.run_script(**env)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.state.read_bytes(), before)

    def test_refusal_does_not_create_checkpoint_and_retry_reuses_ids(self):
        self.assertNotEqual(self.run_script(HTTP="503").returncode, 0)
        self.assertFalse(self.state.exists())
        self.assertEqual(self.run_script().returncode, 0)
        self.assertEqual(self.sent()[0], self.sent()[1])

    def test_explicit_zero_rejected_is_accepted(self):
        response = '{"ok":true,"accepted":2,"rejected":0}'
        self.assertEqual(self.run_script(RESPONSE=response).returncode, 0)
        self.assertEqual(json.loads(self.state.read_text())["event_id"], SECOND)

    def test_invalid_configuration_refused_before_source_or_http(self):
        for env in [{"UMAMI_WEBSITE_ID": "x'; DROP TABLE website_event;--"},
                    {"BATCH": "1; SELECT 1"}, {"BATCH": "101"}, {"BATCH": "01"},
                    {"STOP_AFTER": "-1"}, {"OBSERVE_API_KEY": "key\nInjected: yes"},
                    {"OBSERVE_ENDPOINT": "https://user:secret@observe.invalid"},
                    {"OBSERVE_ENDPOINT": "https://observe.invalid?key=secret"},
                    {"OBSERVE_SITE_ID": "site\n"}]:
            with self.subTest(env=env):
                self.assertNotEqual(self.run_script(**env).returncode, 0)
                self.assertFalse((self.root / "psql-called").exists())
                self.assertEqual(self.sent(), [])

    def test_malicious_checkpoint_refused_before_source(self):
        self.state.write_text("x'; DELETE FROM website_event;--\n")
        self.assertNotEqual(self.run_script().returncode, 0)
        self.assertFalse((self.root / "psql-called").exists())

    def test_destination_change_refused_on_resume(self):
        self.assertEqual(self.run_script(STOP_AFTER="1").returncode, 0)
        before = self.state.read_bytes()
        self.assertNotEqual(self.run_script(OBSERVE_SITE_ID="different").returncode, 0)
        self.assertEqual(self.state.read_bytes(), before)
        self.assertEqual(len(self.sent()), 1)

    def test_invalid_source_batch_refused_before_http(self):
        cases = [[{**self.row(FIRST), "session_id": None}],
                 [{**self.row(FIRST), "timestamp": None}],
                 [{**self.row(FIRST), "pathname": "relative"}],
                 [self.row(SECOND), self.row(FIRST)],
                 [self.row(FIRST), self.row(FIRST)]]
        for rows in cases:
            with self.subTest(rows=rows):
                self.rows = rows
                self.assertNotEqual(self.run_script().returncode, 0)
                self.assertFalse(self.state.exists())
                self.assertEqual(self.sent(), [])

    def test_empty_source_does_not_create_checkpoint(self):
        self.rows = []
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.state.exists())
        self.assertEqual(self.sent(), [])

    def test_checkpoint_symlink_is_not_followed(self):
        other = self.root / "other"
        other.write_text(FIRST + "\n")
        self.state.symlink_to(other)
        self.assertNotEqual(self.run_script().returncode, 0)
        self.assertEqual(other.read_text(), FIRST + "\n")


if __name__ == "__main__":
    if not shutil.which("jq"):
        raise SystemExit("jq is required for importer tests")
    unittest.main()
