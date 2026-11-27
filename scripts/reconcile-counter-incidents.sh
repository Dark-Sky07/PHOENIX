#!/usr/bin/env bash
# Backend-aware, backup-first reconciliation of PHOENIX accounting evidence.
#
# The active x-ui process is the source of truth. In particular, do not infer
# SQLite from /etc/x-ui/x-ui.db when a running panel uses PostgreSQL.
#
# Safety rules enforced below:
#   * dry-run by default; --apply requires the exact confirmation token
#   * runtime backend/folder are read from the active x-ui process when possible
#   * an immutable database backup and exact-ID CSV exports precede every delete
#   * local Xray and L2TP evidence is archived before database barriers clear
#   * only IDs snapshotted by this invocation can be deleted
#   * traffic journals are never removed (they can hold unapplied traffic)
#   * no service stop/restart commands and no Xray process control
set -euo pipefail

confirm_token="RECONCILE_COUNTER_INCIDENTS"
apply=0
confirm=""
dsn_override=""
db_type_override=""
db_folder_override=""

usage() {
  cat <<'USAGE'
Usage:
  reconcile-counter-incidents.sh [--apply --confirm RECONCILE_COUNTER_INCIDENTS]
                                 [--dsn POSTGRES_DSN]
                                 [--db-type postgres|sqlite]
                                 [--db-folder PATH]

Without --apply the script only reports the exact evidence it would archive.
When x-ui is running, its /proc environment determines the active backend,
DSN, and DB folder. If x-ui is stopped, specify --db-type explicitly; for
PostgreSQL also pass --dsn. This script never stops or restarts a service.
USAGE
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --apply) apply=1 ;;
    --confirm) confirm="${2:-}"; shift ;;
    --dsn) dsn_override="${2:-}"; shift ;;
    --db-type) db_type_override="${2:-}"; shift ;;
    --db-folder) db_folder_override="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [ "$(id -u)" -ne 0 ]; then
  die "run as root (sudo -i)"
fi
if [ "$apply" -eq 1 ] && [ "$confirm" != "$confirm_token" ]; then
  die "refusing --apply without --confirm $confirm_token"
fi

# Print the exact key's value from newline-separated KEY=VALUE entries. The
# process environment preserves DSNs containing spaces and equals signs.
env_value() {
  local key="$1" lines="$2" line
  while IFS= read -r line; do
    if [[ "$line" == "$key="* ]]; then
      printf '%s\n' "${line#*=}"
      return 0
    fi
  done <<< "$lines"
  return 1
}

runtime_env=""
runtime_pid="$(systemctl show x-ui -p MainPID --value 2>/dev/null || true)"
if [[ "$runtime_pid" =~ ^[1-9][0-9]*$ ]] && [ -r "/proc/$runtime_pid/environ" ]; then
  runtime_env="$(tr '\0' '\n' < "/proc/$runtime_pid/environ")"
fi

runtime_db_type="$(env_value XUI_DB_TYPE "$runtime_env" || true)"
runtime_dsn="$(env_value XUI_DB_DSN "$runtime_env" || true)"
runtime_db_folder="$(env_value XUI_DB_FOLDER "$runtime_env" || true)"

if [ -n "$db_type_override" ]; then
  db_type="$db_type_override"
elif [ -n "$runtime_env" ]; then
  # SQLite is PHOENIX's documented default only after we successfully inspected
  # the running process and found no XUI_DB_TYPE override.
  db_type="${runtime_db_type:-sqlite}"
elif [ -n "$dsn_override" ]; then
  db_type="postgres"
else
  die "could not inspect a running x-ui process; pass --db-type explicitly (and --dsn for PostgreSQL)"
fi

case "$(printf '%s' "$db_type" | tr '[:upper:]' '[:lower:]')" in
  postgres|postgresql|pg) db_type="postgres" ;;
  sqlite|sqlite3|"") db_type="sqlite" ;;
  *) die "unsupported database type: $db_type" ;;
esac

if [ -n "$db_folder_override" ]; then
  db_folder="$db_folder_override"
elif [ -n "$runtime_db_folder" ]; then
  db_folder="$runtime_db_folder"
else
  db_folder="/etc/x-ui"
fi
[ -d "$db_folder" ] || die "database folder does not exist: $db_folder"

if [ "$db_type" = "postgres" ]; then
  dsn="${dsn_override:-$runtime_dsn}"
  [ -n "$dsn" ] || die "active PostgreSQL DSN was not found; pass --dsn explicitly"
  command -v psql >/dev/null 2>&1 || die "psql is required for PostgreSQL reconciliation"
  if [ "$apply" -eq 1 ]; then
    command -v pg_dump >/dev/null 2>&1 || die "pg_dump is required for an --apply PostgreSQL reconciliation"
  fi
else
  [ -z "$dsn_override" ] || die "--dsn is only valid with --db-type postgres"
  command -v python3 >/dev/null 2>&1 || die "python3 is required for SQLite reconciliation"
  sqlite_path="$db_folder/x-ui.db"
  [ -f "$sqlite_path" ] || die "active SQLite database does not exist: $sqlite_path"
fi

fallback_dir="$db_folder/counter-incident-fallback"
l2tp_marker="$db_folder/l2tp-accounting-incident"
l2tp_evidence_dir="$db_folder/l2tp-accounting-evidence"
xray_journal_dir="$db_folder/xray-traffic-journal"
tuic_journal_dir="$db_folder/tuic-traffic-journal"
incident_table="reseller_node_counter_incidents"
poll_table="xray_traffic_inflights"

sqlite_scalar() {
  python3 - "$sqlite_path" "$1" <<'PY'
import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
try:
    row = connection.execute(sys.argv[2]).fetchone()
    print("" if row is None or row[0] is None else row[0])
finally:
    connection.close()
PY
}

sqlite_export_csv() {
  local table="$1" ids="$2" outfile="$3" order_by="$4"
  python3 - "$sqlite_path" "$table" "$ids" "$outfile" "$order_by" <<'PY'
import csv, sqlite3, sys
path, table, ids, output, order_by = sys.argv[1:]
if table not in {"reseller_node_counter_incidents", "xray_traffic_inflights"}:
    raise SystemExit("refusing unexpected SQLite table")
if table == "reseller_node_counter_incidents":
    keys = [int(v) for v in ids.split(",") if v]
else:
    keys = [v for v in ids.split(",") if v]
connection = sqlite3.connect(path)
try:
    cur = connection.cursor()
    if keys:
        placeholders = ",".join("?" for _ in keys)
        cur.execute(f"SELECT * FROM {table} WHERE id IN ({placeholders}) ORDER BY {order_by}", keys)
    else:
        cur.execute(f"SELECT * FROM {table} WHERE 1=0")
    with open(output, "w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow([col[0] for col in cur.description])
        writer.writerows(cur)
finally:
    connection.close()
PY
}

sqlite_backup() {
  python3 - "$sqlite_path" "$1" <<'PY'
import sqlite3, sys
source = sqlite3.connect(sys.argv[1])
target = sqlite3.connect(sys.argv[2])
try:
    source.backup(target)
finally:
    target.close()
    source.close()
PY
}

sqlite_delete_ids() {
  local table="$1" ids="$2"
  python3 - "$sqlite_path" "$table" "$ids" <<'PY'
import sqlite3, sys
path, table, ids = sys.argv[1:]
if table not in {"reseller_node_counter_incidents", "xray_traffic_inflights"}:
    raise SystemExit("refusing unexpected SQLite table")
if table == "reseller_node_counter_incidents":
    keys = [int(v) for v in ids.split(",") if v]
else:
    keys = [v for v in ids.split(",") if v]
if not keys:
    raise SystemExit(0)
connection = sqlite3.connect(path)
try:
    connection.execute("BEGIN IMMEDIATE")
    placeholders = ",".join("?" for _ in keys)
    cur = connection.execute(f"DELETE FROM {table} WHERE id IN ({placeholders})", keys)
    if cur.rowcount != len(keys):
        connection.rollback()
        raise SystemExit(f"{table} changed during reconciliation: deleted {cur.rowcount}, expected {len(keys)}")
    connection.commit()
finally:
    connection.close()
PY
}

if [ "$db_type" = "postgres" ]; then
  incident_ids="$(psql "$dsn" -X -qAt -v ON_ERROR_STOP=1 -c "SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM $incident_table" | tr -d '[:space:]')"
  poll_exists="$(psql "$dsn" -X -qAt -v ON_ERROR_STOP=1 -c "SELECT to_regclass('$poll_table') IS NOT NULL")"
  if [ "$poll_exists" = "t" ]; then
    poll_ids="$(psql "$dsn" -X -qAt -v ON_ERROR_STOP=1 -c "SELECT COALESCE(string_agg(id, ',' ORDER BY id), '') FROM $poll_table" | tr -d '[:space:]')"
  else
    poll_ids=""
    echo "NOTE: table $poll_table does not exist; no poll markers to clear"
  fi
else
  incident_ids="$(sqlite_scalar "SELECT COALESCE(group_concat(id, ','), '') FROM (SELECT id FROM $incident_table ORDER BY id)")"
  poll_exists="$(sqlite_scalar "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='$poll_table')")"
  if [ "$poll_exists" = "1" ]; then
    poll_ids="$(sqlite_scalar "SELECT COALESCE(group_concat(id, ','), '') FROM (SELECT id FROM $poll_table ORDER BY id)")"
  else
    poll_ids=""
    echo "NOTE: table $poll_table does not exist; no poll markers to clear"
  fi
fi

count_ids() {
  if [ -z "$1" ]; then printf '0\n'; else awk -F, '{print NF}' <<< "$1"; fi
}
incidents="$(count_ids "$incident_ids")"
polls="$(count_ids "$poll_ids")"

# Reconciliation archives only regular files. Anything else in an evidence
# directory is an integrity problem, not a file for a root script to move.
regular_file_names() {
  local directory="$1" entry
  [ -d "$directory" ] || return 0
  while IFS= read -r -d '' entry; do
    [ -f "$entry" ] && [ ! -L "$entry" ] || die "unexpected non-regular evidence entry: $entry"
    printf '%s\0' "${entry##*/}"
  done < <(find "$directory" -mindepth 1 -maxdepth 1 -print0)
}

fallback_files=()
while IFS= read -r -d '' name; do fallback_files+=("$name"); done < <(regular_file_names "$fallback_dir")
l2tp_evidence_files=()
while IFS= read -r -d '' name; do l2tp_evidence_files+=("$name"); done < <(regular_file_names "$l2tp_evidence_dir")

l2tp_marker_present=0
if [ -e "$l2tp_marker" ] || [ -L "$l2tp_marker" ]; then
  [ -f "$l2tp_marker" ] && [ ! -L "$l2tp_marker" ] || die "invalid L2TP accounting marker entry: $l2tp_marker"
  l2tp_marker_present=1
fi

xray_journal_count=0
tuic_journal_count=0
[ -d "$xray_journal_dir" ] && xray_journal_count="$(find "$xray_journal_dir" -maxdepth 1 -type f | wc -l | tr -d ' ')"
[ -d "$tuic_journal_dir" ] && tuic_journal_count="$(find "$tuic_journal_dir" -maxdepth 1 -type f | wc -l | tr -d ' ')"

echo "backend=$db_type folder=$db_folder"
echo "incidents=$incidents polls=$polls fallback_files=${#fallback_files[@]} l2tp_marker=$l2tp_marker_present l2tp_evidence_files=${#l2tp_evidence_files[@]}"
echo "xray_journal_files=$xray_journal_count tuic_journal_files=$tuic_journal_count"
echo "note: traffic journal files are never removed by this script"

if [ "$apply" -ne 1 ]; then
  echo "DRY_RUN=1 no evidence changed"
  exit 0
fi

backup_root="$db_folder/reconciliation-backups"
backup="$backup_root/cli-$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 0700 "$backup"

if [ "$db_type" = "postgres" ]; then
  pg_dump --format=custom --no-owner --no-privileges --dbname="$dsn" --file="$backup/postgres-before-reconciliation.dump"
  if [ -n "$incident_ids" ]; then
    psql "$dsn" -X -q -v ON_ERROR_STOP=1 -v incident_ids="$incident_ids" -c "COPY (SELECT * FROM $incident_table WHERE id = ANY(string_to_array(:'incident_ids', ',')::integer[]) ORDER BY id) TO STDOUT WITH CSV HEADER" > "$backup/counter-incidents.csv"
  else
    psql "$dsn" -X -q -v ON_ERROR_STOP=1 -c "COPY (SELECT * FROM $incident_table WHERE FALSE ORDER BY id) TO STDOUT WITH CSV HEADER" > "$backup/counter-incidents.csv"
  fi
  if [ "$poll_exists" = "t" ]; then
    if [ -n "$poll_ids" ]; then
      psql "$dsn" -X -q -v ON_ERROR_STOP=1 -v poll_ids="$poll_ids" -c "COPY (SELECT * FROM $poll_table WHERE id = ANY(string_to_array(:'poll_ids', ',')) ORDER BY created_at) TO STDOUT WITH CSV HEADER" > "$backup/in-flight-polls.csv"
    else
      psql "$dsn" -X -q -v ON_ERROR_STOP=1 -c "COPY (SELECT * FROM $poll_table WHERE FALSE ORDER BY created_at) TO STDOUT WITH CSV HEADER" > "$backup/in-flight-polls.csv"
    fi
  fi
else
  sqlite_backup "$backup/sqlite-before-reconciliation.db"
  sqlite_export_csv "$incident_table" "$incident_ids" "$backup/counter-incidents.csv" "id"
  if [ "$poll_exists" = "1" ]; then
    sqlite_export_csv "$poll_table" "$poll_ids" "$backup/in-flight-polls.csv" "created_at"
  fi
fi

if [ "${#fallback_files[@]}" -gt 0 ]; then
  install -d -m 0700 "$backup/counter-incident-fallback"
  for name in "${fallback_files[@]}"; do
    [ -f "$fallback_dir/$name" ] && [ ! -L "$fallback_dir/$name" ] || die "fallback evidence changed during reconciliation: $name"
    mv -- "$fallback_dir/$name" "$backup/counter-incident-fallback/$name"
  done
fi
if [ "$l2tp_marker_present" -eq 1 ]; then
  [ -f "$l2tp_marker" ] && [ ! -L "$l2tp_marker" ] || die "L2TP marker changed during reconciliation"
  mv -- "$l2tp_marker" "$backup/l2tp-accounting-incident"
fi
if [ "${#l2tp_evidence_files[@]}" -gt 0 ]; then
  install -d -m 0700 "$backup/l2tp-accounting-evidence"
  for name in "${l2tp_evidence_files[@]}"; do
    [ -f "$l2tp_evidence_dir/$name" ] && [ ! -L "$l2tp_evidence_dir/$name" ] || die "L2TP evidence changed during reconciliation: $name"
    mv -- "$l2tp_evidence_dir/$name" "$backup/l2tp-accounting-evidence/$name"
  done
fi
sync

# A new L2TP observation can reuse the shared -2 database row. It must prevent
# deletion of every snapshotted incident rather than being mistaken for the old
# evidence just archived above.
current_l2tp_files=()
while IFS= read -r -d '' name; do current_l2tp_files+=("$name"); done < <(regular_file_names "$l2tp_evidence_dir")
if [ -e "$l2tp_marker" ] || [ -L "$l2tp_marker" ] || [ "${#current_l2tp_files[@]}" -ne 0 ]; then
  die "fresh L2TP accounting evidence appeared during reconciliation; database rows were retained"
fi

if [ -n "$incident_ids" ]; then
  if [ "$db_type" = "postgres" ]; then
    psql "$dsn" -X -v ON_ERROR_STOP=1 -v incident_ids="$incident_ids" -c "DELETE FROM $incident_table WHERE id = ANY(string_to_array(:'incident_ids', ',')::integer[]);"
  else
    sqlite_delete_ids "$incident_table" "$incident_ids"
  fi
fi
if [ -n "$poll_ids" ]; then
  if [ "$db_type" = "postgres" ]; then
    psql "$dsn" -X -v ON_ERROR_STOP=1 -v poll_ids="$poll_ids" -c "DELETE FROM $poll_table WHERE id = ANY(string_to_array(:'poll_ids', ','));"
  else
    sqlite_delete_ids "$poll_table" "$poll_ids"
  fi
fi

cat > "$backup/recovery-manifest.txt" <<EOF
backend=$db_type
incident_ids=${incident_ids:-none}
poll_ids=${poll_ids:-none}
fallback_files=${#fallback_files[@]}
l2tp_marker=$l2tp_marker_present
l2tp_evidence_files=${#l2tp_evidence_files[@]}
traffic_journals=not modified
completed_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
sync

echo "BACKUP=$backup"
echo "RECONCILED=1 service was not stopped or restarted"
echo "VERIFY: refresh Resellers and online clients; inspect journalctl -u x-ui"
