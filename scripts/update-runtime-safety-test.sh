#!/usr/bin/env bash
# Runtime regression for the deleted-CWD update failure. Source update.sh's
# opt-in harness and use a fake inactive systemd unit so no host service is
# touched. A managed sidecar must be reaped before a swap; an unrelated process
# merely parked in the panel CWD must make the updater refuse the swap.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root_dir/update.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

install_root="$tmp/x-ui"
fakebin="$tmp/fakebin"
mkdir -p "$install_root/bin" "$fakebin" "$tmp/systemd"
cat > "$fakebin/systemctl" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${PHOENIX_FAKE_SYSTEMCTL_LOG}"
case "${1:-}" in
  stop|daemon-reload) exit 0 ;;
  is-active|is-enabled) exit 3 ;;
  enable|disable)
    echo "unexpected systemctl ${1}: unit enablement must survive the atomic unit replacement" >&2
    exit 93
    ;;
  *) exit 0 ;;
esac
SH
chmod +x "$fakebin/systemctl"

# The sourced updater needs only its function definitions. It must not perform
# package installation, downloads, or touch the system service in this mode.
export PHOENIX_UPDATE_TEST_LIB=1
export XUI_MAIN_FOLDER="$install_root"
export XUI_SERVICE="$tmp/systemd"
export PHOENIX_UPDATE_TEST_XUI_COMMAND_PATH="$fakebin/x-ui"
export PHOENIX_UPDATE_TEST_PHOENIX_COMMAND_PATH="$fakebin/phoenix"
export PHOENIX_FAKE_SYSTEMCTL_LOG="$tmp/systemctl.log"
# shellcheck disable=SC1090
source "$script"
release=debian
xui_folder="$install_root"
PATH="$tmp/fakebin:$PATH"

# A process whose executable is inside the install tree is a managed panel
# sidecar. The stop guard is allowed to terminate it and must wait until the
# process no longer holds the CWD before it reports success.
cp "$(command -v sleep)" "$install_root/bin/mtg-linux-amd64"
(
  cd "$install_root"
  exec "$install_root/bin/mtg-linux-amd64" 30
) &
managed_pid=$!
sleep 0.1
service_stop_for_update
if kill -0 "$managed_pid" 2>/dev/null; then
  echo "managed panel sidecar survived the service-stop guard" >&2
  exit 1
fi
if [[ -n "$(live_install_processes)" ]]; then
  echo "live install process remained after managed-sidecar quiescence" >&2
  exit 1
fi

# A process with an unrelated executable may be an administrator shell or a
# foreign workload. It must never be killed merely because its CWD is the panel
# directory; the updater must abort before rename/remove can create a deleted
# working directory.
(
  cd "$install_root"
  exec sleep 30
) &
unmanaged_pid=$!
cleanup_unmanaged() { kill "$unmanaged_pid" 2>/dev/null || true; }
trap 'cleanup_unmanaged; rm -rf "$tmp"' EXIT
sleep 0.1
if service_stop_for_update; then
  echo "service-stop guard accepted an unrelated process holding panel CWD" >&2
  exit 1
fi
if ! kill -0 "$unmanaged_pid" 2>/dev/null; then
  echo "service-stop guard killed an unrelated CWD holder" >&2
  exit 1
fi

# A disabled service has no enablement link to recreate. Replacing the unit at
# its stable path must therefore not run `systemctl disable`: several systemd
# releases report that harmless no-op as an error, which used to force a full
# rollback after the live tree had already been swapped. Exercise the whole
# commit boundary with private test-only command paths and make either command
# fail loudly if it is invoked.
tx_root="$tmp/transaction"
tx_live="$tx_root/live/x-ui"
tx_stage_root="$tx_root/stage"
tx_stage_dir="$tx_stage_root/x-ui"
tx_service_dir="$tx_root/systemd"
mkdir -p "$tx_live" "$tx_stage_dir" "$tx_service_dir"
printf '%s\n' old-panel > "$tx_live/x-ui"
printf '%s\n' candidate-panel > "$tx_stage_dir/x-ui"
printf '%s\n' old-wrapper > "$fakebin/x-ui"
printf '%s\n' old-unit > "$tx_service_dir/x-ui.service"
printf '%s\n' candidate-wrapper > "$tx_stage_root/wrapper"
printf '%s\n' candidate-unit > "$tx_stage_root/x-ui.service"
chmod +x "$tx_stage_root/wrapper"

xui_folder="$tx_live"
xui_service="$tx_service_dir"
release=ubuntu
update_stage_root="$tx_stage_root"
update_stage_dir="$tx_stage_dir"
update_wrapper_stage="$tx_stage_root/wrapper"
update_service_stage="$tx_stage_root/x-ui.service"
update_maintenance_active=1
update_commit_started=0
update_old_service_active=0
update_old_service_enabled=0
update_db_snapshot_ready=0
snapshot_update_database() { update_db_snapshot_ready=1; }

if ! commit_update_transaction; then
  echo "transactional commit rejected an otherwise valid disabled service" >&2
  exit 1
fi
if [[ "$(cat "$tx_live/x-ui")" != "candidate-panel" ]]; then
  echo "transactional commit did not install the staged panel" >&2
  exit 1
fi
if [[ "$(cat "$fakebin/x-ui")" != "candidate-wrapper" ]]; then
  echo "transactional commit did not install the staged management wrapper" >&2
  exit 1
fi
if grep -Eq '^(enable|disable) ' "$PHOENIX_FAKE_SYSTEMCTL_LOG"; then
  echo "transactional commit mutated service enablement during unit replacement" >&2
  exit 1
fi

# Restore the sourced function for callers below this test process. It is not
# used again today, but keeping the harness state coherent prevents accidental
# coupling when this script gains more cases.
unset -f snapshot_update_database

echo "update runtime CWD and disabled-service transaction safety checks passed"
