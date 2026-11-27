#!/usr/bin/env bash
# Backup-first reconciliation of PHOENIX counter-incident markers.
#
# Safety rules enforced below:
#   * dry-run by default; --apply requires the exact confirmation token
#   * no service stop/restart commands and no Xray process control
#   * evidence is copied/moved into a timestamped backup before any DELETE
#   * traffic journals are never removed (they can hold unapplied traffic)
set -euo pipefail

confirm_token="RECONCILE_COUNTER_INCIDENTS"
apply=0
confirm=""
dsn_override=""

usage() {
  cat <<'USAGE'
Usage: reconcile-counter-incidents.sh [--apply --confirm RECONCILE_COUNTER_INCIDENTS] [--dsn DSN]
Without --apply the script only prints what would be changed.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --apply) apply=1 ;;
    --confirm) confirm="${2:-}"; shift ;;
    --dsn) dsn_override="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (sudo -i)" >&2
  exit 1
fi

if [ "$apply" -eq 1 ] && [ "$confirm" != "$confirm_token" ]; then
  echo "refusing --apply without --confirm $confirm_token" >&2
  exit 2
fi

if ! command -v psql >/dev/null 2>&1; then
  echo "psql is required (install postgresql-client)" >&2
  exit 1
fi

dsn="$dsn_override"
if [ -z "$dsn" ]; then
  envs="$(systemctl show x-ui -p Environment --value 2>/dev/null || true)"
  for file in /etc/default/x-ui /etc/conf.d/x-ui /etc/sysconfig/x-ui; do
    if [ -f "$file" ]; then
      envs="$envs
$(cat "$file")"
    fi
  done
  dsn="$(printf '%s\n' "$envs" | tr ' ' '\n' | grep '^XUI_DB_DSN=' | head -n 1 | cut -d= -f2- | tr -d '"' || true)"
fi
if [ -z "$dsn" ]; then
  echo "XUI_DB_DSN not found; pass --dsn DSN" >&2
  exit 1
fi

db_folder="${XUI_DB_FOLDER:-/etc/x-ui}"
fallback_dir="$db_folder/counter-incident-fallback"
xray_journal_dir="$db_folder/xray-traffic-journal"
tuic_journal_dir="$db_folder/tuic-traffic-journal"
incident_table="reseller_node_counter_incidents"
poll_table="xray_traffic_inflights"

# Snapshot exact primary keys before any backup or repair. Another panel can
# persist new evidence while this operator reviews the output; that later row
# must never be deleted by this run because it is absent from this snapshot.
incident_ids="$(psql "$dsn" -tAc "SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM $incident_table" | tr -d '[:space:]')"
incidents=0
if [ -n "$incident_ids" ]; then
  incidents="$(printf '%s' "$incident_ids" | awk -F, '{print NF}')"
fi
poll_ids=""
polls="0"
if psql "$dsn" -tAc "SELECT to_regclass('public.$poll_table') IS NOT NULL" | grep -qx t; then
  poll_ids="$(psql "$dsn" -tAc "SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM $poll_table" | tr -d '[:space:]')"
  if [ -n "$poll_ids" ]; then
    polls="$(printf '%s' "$poll_ids" | awk -F, '{print NF}')"
  fi
else
  echo "NOTE: table $poll_table does not exist; no poll markers to clear"
fi
fallback_files=()
if [ -d "$fallback_dir" ]; then
  while IFS= read -r -d '' file; do
    fallback_files+=("${file##*/}")
  done < <(find "$fallback_dir" -maxdepth 1 -mindepth 1 \( -type f -o -type l \) -print0)
fi
fallback_count="${#fallback_files[@]}"
xray_journal_count=0
tuic_journal_count=0
if [ -d "$xray_journal_dir" ]; then
  xray_journal_count="$(find "$xray_journal_dir" -maxdepth 1 -type f | wc -l | tr -d ' ')"
fi
if [ -d "$tuic_journal_dir" ]; then
  tuic_journal_count="$(find "$tuic_journal_dir" -maxdepth 1 -type f | wc -l | tr -d ' ')"
fi

echo "incidents=$incidents polls=$polls fallback_files=$fallback_count"
echo "xray_journal_files=$xray_journal_count tuic_journal_files=$tuic_journal_count"
echo "note: journal files are never removed by this script"

if [ "$apply" -ne 1 ]; then
  echo "DRY_RUN=1 no evidence changed"
  exit 0
fi

backup_root="$db_folder/reconciliation-backups"
backup="$backup_root/cli-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$backup"

psql "$dsn" -c "COPY (SELECT * FROM $incident_table ORDER BY id) TO STDOUT WITH CSV HEADER" > "$backup/counter-incidents.csv"
if psql "$dsn" -tAc "SELECT to_regclass('public.$poll_table') IS NOT NULL" | grep -qx t; then
  psql "$dsn" -c "COPY (SELECT * FROM $poll_table ORDER BY created_at) TO STDOUT WITH CSV HEADER" > "$backup/in-flight-polls.csv"
fi

if [ "$fallback_count" -gt 0 ]; then
  mkdir -p "$backup/counter-incident-fallback"
  for name in "${fallback_files[@]}"; do
    source="$fallback_dir/$name"
    target="$backup/counter-incident-fallback/$name"
    # A new incident may have been written after the initial listing. Move
    # only the exact snapshotted entry, and preserve symlinks as evidence
    # instead of following them while running as root.
    if [ -e "$source" ] || [ -L "$source" ]; then
      cp -Pp -- "$source" "$target"
      mv -- "$source" "$target"
    fi
  done
fi

echo "BACKUP=$backup"
if [ -n "$incident_ids" ]; then
  psql "$dsn" -v incident_ids="$incident_ids" -c "DELETE FROM $incident_table WHERE id = ANY(string_to_array(:'incident_ids', ',')::integer[]);"
fi
if [ -n "$poll_ids" ] && psql "$dsn" -tAc "SELECT to_regclass('public.$poll_table') IS NOT NULL" | grep -qx t; then
  psql "$dsn" -v poll_ids="$poll_ids" -c "DELETE FROM $poll_table WHERE id = ANY(string_to_array(:'poll_ids', ','));"
fi

echo "RECONCILED=1 service was not stopped or restarted"
echo "VERIFY: refresh Resellers and online clients; inspect journalctl -u x-ui"
