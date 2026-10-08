#!/usr/bin/env python3
"""Offline relational copy/recovery tests for migrations 064/065.

Run: python3 internal/schema/migration_tails_test.py
SQLite executes the column/copy SQL after removing Nucleus storage clauses.
DDL is executed in autocommit mode to model the documented nontransactional
failure boundaries. This does not establish Nucleus engine acceptance.
"""
from pathlib import Path
import re
import sqlite3
import unittest

ROOT = Path(__file__).with_name("migrations")
CASES = [
    ("metric_points", "022_metrics.up.sql", "064_metrics_stream_identity.up.sql", "stream_identity"),
    ("performance_issues", "015_performance_issues.up.sql", "065_performance_issue_version.up.sql", "version"),
]


def statements(name):
    sql = (ROOT / name).read_text()
    sql = re.sub(r"--[^\n]*", "", sql)
    # SQLite tests row copy semantics, not MergeTree replacement/storage.
    sql = re.sub(r"\)\s+WITH\s*\(.*?\)\s*ORDER BY\s*\([^;]*\)", ")", sql, flags=re.S)
    return [s.strip() for s in sql.split(";") if s.strip()]


def rows(db, table):
    return db.execute(f"SELECT * FROM {table}").fetchall()


class MigrationTailsTests(unittest.TestCase):
    def fixture(self, table, old):
        db = sqlite3.connect(":memory:", isolation_level=None)
        self.addCleanup(db.close)
        for stmt in statements(old):
            db.execute(stmt)
        if table == "metric_points":
            db.executemany("INSERT INTO metric_points VALUES (?,?,?,?,?,?,?,?,?,?,?)", [
                ("site", "tenant", "requests", "sum", "service-A", '{"line":"\\t\\n雪"}',
                 1700000000000000000, 123.5, "", "true", "cumulative"),
                ("site", "tenant", "latency", "histogram", "service-B", '{}',
                 1700000001000000000, 0, '{"bounds":[1],"counts":[3,4],"sum":9,"count":7}', "false", "delta"),
                # Duplicate physical points must not be grouped or lost.
                ("site", "tenant", "requests", "sum", "service-A", '{"line":"\\t\\n雪"}',
                 1700000000000000000, 123.5, "", "true", "cumulative"),
            ])
        else:
            db.executemany("INSERT INTO performance_issues VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", [
                ("id-1", "tenant", "site", "trace-1", "slow_db", "fingerprint", "title", "original\n雪", "warning", 7, 3000, 5100),
                ("id-2", "tenant", "site", "trace-2", "slow_db", "fingerprint", "title", "later evidence", "error", 8, 2000, 5100),
                ("id-3", "tenant-other", "site", "trace-3", "slow_db", "fingerprint", "title", "other tenant", "info", 2, 4000, 4900),
            ])
        return db

    def test_populated_copy_preserves_every_original_column_and_row(self):
        for table, old, new, added in CASES:
            with self.subTest(migration=new):
                db = self.fixture(table, old)
                before = rows(db, table)
                for stmt in statements(new):
                    db.execute(stmt)
                after = rows(db, table)
                self.assertEqual([row[:-1] for row in after], before)
                self.assertEqual(rows(db, table + "_pre" + new[:3]), before)
                expected = ["" for row in before] if added == "stream_identity" else [row[-1] for row in before]
                self.assertEqual([row[-1] for row in after], expected)
                db.execute(f"UPDATE {table} SET {added} = {added} WHERE 1=0")

    def test_retry_after_each_persistent_step_refuses_without_losing_source(self):
        for table, old, new, added in CASES:
            plan = statements(new)
            self.assertEqual(len(plan), 3, "review new failure boundaries if migration changes")
            for boundary in range(1, len(plan) + 1):
                with self.subTest(migration=new, boundary=boundary):
                    db = self.fixture(table, old)
                    before = rows(db, table)
                    aside = table + "_pre" + new[:3]
                    # Simulate process death before the history write. No DDL rollback.
                    for stmt in plan[:boundary]:
                        db.execute(stmt)
                    with self.assertRaises(sqlite3.OperationalError):
                        for stmt in plan:
                            db.execute(stmt)
                    self.assertEqual(rows(db, aside), before)
                    if boundary > 1:
                        self.assertEqual(len(rows(db, table)), 0 if boundary == 2 else len(before))
                    # Documented recovery with writers stopped and absent history.
                    db.execute(f"DROP TABLE IF EXISTS {table}")
                    db.execute(f"ALTER TABLE {aside} RENAME TO {table}")
                    for stmt in plan:
                        db.execute(stmt)
                    self.assertEqual([r[:-1] for r in rows(db, table)], before)
                    self.assertEqual(rows(db, aside), before)

    def test_independent_version_makes_late_and_equal_time_snapshots_visible(self):
        table, old, new, _ = CASES[1]
        db = self.fixture(table, old)
        for stmt in statements(new):
            db.execute(stmt)
        # The detector writes count/min/max separately from the increasing version.
        for version, count, first in [(6000, 9, 1000), (6001, 10, 500)]:
            db.execute("INSERT INTO performance_issues VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                       ("id-1", "tenant", "site", "late-trace", "slow_db", "fingerprint", "title", "late evidence", "error", count, first, 5100, version))
            snapshot = db.execute("SELECT count, first_seen, last_seen, version, description FROM performance_issues WHERE tenant_id='tenant' ORDER BY version DESC LIMIT 1").fetchone()
            self.assertEqual(snapshot, (count, first, 5100, version, "late evidence"))

    def test_metric_default_and_new_identity_survive_new_writes(self):
        table, old, new, _ = CASES[0]
        db = self.fixture(table, old)
        for stmt in statements(new):
            db.execute(stmt)
        db.execute("INSERT INTO metric_points (site_id, metric_name, metric_kind, ts_ns) VALUES ('new', 'gauge', 'gauge', 42)")
        self.assertEqual(db.execute("SELECT stream_identity FROM metric_points WHERE site_id='new'").fetchone(), ("",))
        db.execute("INSERT INTO metric_points (site_id, metric_name, metric_kind, ts_ns, stream_identity) VALUES ('new2', 'gauge', 'gauge', 43, 'resource-hash')")
        self.assertEqual(db.execute("SELECT stream_identity FROM metric_points WHERE site_id='new2'").fetchone(), ("resource-hash",))


if __name__ == "__main__":
    unittest.main()
