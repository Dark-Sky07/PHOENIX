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
mkdir -p "$install_root/bin" "$tmp/fakebin"
cat > "$tmp/fakebin/systemctl" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in
  stop) exit 0 ;;
  is-active) exit 3 ;;
  *) exit 0 ;;
esac
SH
chmod +x "$tmp/fakebin/systemctl"

# The sourced updater needs only its function definitions. It must not perform
# package installation, downloads, or touch the system service in this mode.
export PHOENIX_UPDATE_TEST_LIB=1
export XUI_MAIN_FOLDER="$install_root"
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

echo "update runtime CWD safety checks passed"
