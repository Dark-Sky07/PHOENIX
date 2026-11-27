#!/usr/bin/env bash
# Safety and fixture guard for the backend-aware counter-incident recovery CLI.
# PostgreSQL wiring is checked structurally here; SQLite runs against a complete
# temporary fixture so the same archive/delete sequence is exercised in CI.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root_dir/scripts/reconcile-counter-incidents.sh"

bash -n "$script"

python3 - "$script" <<'PY'
from pathlib import Path
import sys

text = Path(sys.argv[1]).read_text()

def forbid(needle: str) -> None:
    if needle in text:
        raise SystemExit(f"reconciliation script must not contain: {needle}")

def require(needle: str) -> None:
    if needle not in text:
        raise SystemExit(f"reconciliation script is missing guard: {needle}")

def order(first: str, second: str) -> None:
    if text.index(first) >= text.index(second):
        raise SystemExit(f"expected {first!r} before {second!r}")

# A mistaken or automated repair must never stop the panel, restart Xray, or
# delete traffic journals that may contain unapplied usage.
forbidden = [
    "systemctl stop",
    "systemctl restart",
    "service x-ui stop",
    "service x-ui restart",
    "pkill",
    "killall",
    "xray-traffic-journal/\" -type f -exec rm",
    "tuic-traffic-journal/\" -type f -exec rm",
    "rm -rf \"$xray_journal_dir\"",
    "rm -rf \"$tuic_journal_dir\"",
]
for needle in forbidden:
    forbid(needle)

require('confirm_token="RECONCILE_COUNTER_INCIDENTS"')
require('apply=0')
require('if [ "$apply" -ne 1 ]; then')
require('echo "DRY_RUN=1 no evidence changed"')
require('tr \'\\0\' \'\\n\' < "/proc/$runtime_pid/environ"')
require('could not inspect a running x-ui process; pass --db-type explicitly')
require('poll_table="xray_traffic_inflights"')
forbid('xray_traffic_in_flight"')
require('pg_dump --format=custom --no-owner --no-privileges')
require('sqlite_backup "$backup/sqlite-before-reconciliation.db"')
require('l2tp_marker="$db_folder/l2tp-accounting-incident"')
require('l2tp_evidence_dir="$db_folder/l2tp-accounting-evidence"')
require('fresh L2TP accounting evidence appeared during reconciliation; database rows were retained')
require('DELETE FROM $incident_table WHERE id = ANY(string_to_array(:\'incident_ids\', \',\')::integer[]);')
require('DELETE FROM $poll_table WHERE id = ANY(string_to_array(:\'poll_ids\', \',\'));')
require('echo "RECONCILED=1 service was not stopped or restarted"')
order('pg_dump --format=custom', "DELETE FROM $incident_table WHERE id = ANY(string_to_array(:'incident_ids', ',')::integer[]);")
order('sqlite_backup "$backup/sqlite-before-reconciliation.db"', 'sqlite_delete_ids "$incident_table" "$incident_ids"')
order('mv -- "$l2tp_marker" "$backup/l2tp-accounting-incident"', "DELETE FROM $incident_table WHERE id = ANY(string_to_array(:'incident_ids', ',')::integer[]);")

print("counter-incident reconciliation static checks passed")
PY

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
python3 - "$tmp/x-ui.db" <<'PY'
import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.executescript('''
CREATE TABLE reseller_node_counter_incidents (
    id INTEGER PRIMARY KEY,
    node_id INTEGER NOT NULL,
    direction TEXT,
    email TEXT NOT NULL DEFAULT '',
    created_at INTEGER
);
CREATE TABLE xray_traffic_inflights (id TEXT PRIMARY KEY, created_at INTEGER);
INSERT INTO reseller_node_counter_incidents VALUES (2, 0, 'api-unreachable', '', 100);
INSERT INTO reseller_node_counter_incidents VALUES (3, -2, 'l2tp-orphan', '', 101);
INSERT INTO xray_traffic_inflights VALUES ('poll-1', 102);
''')
connection.commit()
connection.close()
PY
mkdir -p "$tmp/counter-incident-fallback" "$tmp/l2tp-accounting-evidence" "$tmp/fakebin"
printf 'fallback\n' > "$tmp/counter-incident-fallback/xray.json"
printf 'marker\n' > "$tmp/l2tp-accounting-incident"
printf 'evidence\n' > "$tmp/l2tp-accounting-evidence/l2tp.txt"
printf '#!/usr/bin/env bash\necho 0\n' > "$tmp/fakebin/id"
chmod +x "$tmp/fakebin/id"

PATH="$tmp/fakebin:$PATH" "$script" --db-type sqlite --db-folder "$tmp" > "$tmp/dry-run.txt"
grep -Fx 'DRY_RUN=1 no evidence changed' "$tmp/dry-run.txt" >/dev/null
PATH="$tmp/fakebin:$PATH" "$script" --db-type sqlite --db-folder "$tmp" --apply --confirm RECONCILE_COUNTER_INCIDENTS > "$tmp/apply.txt"

python3 - "$tmp" <<'PY'
from pathlib import Path
import sqlite3, sys

root = Path(sys.argv[1])
connection = sqlite3.connect(root / 'x-ui.db')
try:
    assert connection.execute('SELECT COUNT(*) FROM reseller_node_counter_incidents').fetchone()[0] == 0
    assert connection.execute('SELECT COUNT(*) FROM xray_traffic_inflights').fetchone()[0] == 0
finally:
    connection.close()

backup = next((root / 'reconciliation-backups').iterdir())
for relative in (
    'sqlite-before-reconciliation.db',
    'counter-incidents.csv',
    'in-flight-polls.csv',
    'counter-incident-fallback/xray.json',
    'l2tp-accounting-incident',
    'l2tp-accounting-evidence/l2tp.txt',
    'recovery-manifest.txt',
):
    assert (backup / relative).is_file(), relative
assert not (root / 'counter-incident-fallback/xray.json').exists()
assert not (root / 'l2tp-accounting-incident').exists()
assert not (root / 'l2tp-accounting-evidence/l2tp.txt').exists()
PY

echo "counter-incident SQLite fixture checks passed"
