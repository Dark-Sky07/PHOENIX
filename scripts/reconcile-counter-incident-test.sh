#!/usr/bin/env bash
# Static guard for the backup-first counter-incident reconciliation script.
# The real script is intentionally not run in CI: it needs a live PostgreSQL DSN.
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
require('poll_table="xray_traffic_inflights"')
forbid('xray_traffic_in_flight"')
require('COPY (SELECT * FROM $incident_table ORDER BY id) TO STDOUT WITH CSV HEADER')
require('find "$fallback_dir" -maxdepth 1 -type f -exec cp -p')
require('find "$fallback_dir" -maxdepth 1 -type f -exec mv -t')
require('DELETE FROM $incident_table;')
require('echo "RECONCILED=1 service was not stopped or restarted"')
order('mkdir -p "$backup"', 'DELETE FROM $incident_table;')
order('COPY (SELECT * FROM $incident_table ORDER BY id) TO STDOUT WITH CSV HEADER', 'DELETE FROM $incident_table;')

print("counter-incident reconciliation static checks passed")
PY
