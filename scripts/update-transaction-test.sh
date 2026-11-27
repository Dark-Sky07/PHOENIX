#!/usr/bin/env bash
# Static guard for update.sh's failure boundary. The real update is intentionally
# not run in CI: it downloads release assets and controls a host service.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root_dir}/update.sh"

bash -n "$script" "${root_dir}/install.sh" "${root_dir}/install-phoenix.sh"

python3 - "$script" <<'PY'
from pathlib import Path
import sys

text = Path(sys.argv[1]).read_text()

def require(needle: str) -> None:
    if needle not in text:
        raise SystemExit(f"missing update transaction guard: {needle}")

def order(first: str, second: str) -> None:
    a = text.index(first)
    b = text.index(second)
    if a >= b:
        raise SystemExit(f"expected {first!r} before {second!r}")

# A failed candidate must have an EXIT trap and a database snapshot independent
# of the panel-directory swap. The snapshot covers SQLite sidecars and the
# PostgreSQL dump/restore pair.
require("trap 'rollback_update' EXIT")
require("snapshot_update_database()")
require("restore_update_database()")
require('pg_dump --format=custom')
require('pg_restore --clean --if-exists')
require('for suffix in "" "-wal" "-shm" "-journal"; do')
require("update_db_snapshot_ready=0")

# The old service is stopped before the snapshot, migration is checked before
# the candidate is started, and rollback restores DB state before restarting the
# old service.
order("if ! service_stop_for_update; then return 1; fi", "if ! snapshot_update_database; then")
order("if ! snapshot_update_database; then", "if ! mv \"$xui_folder\" \"$update_backup_dir\"; then")
order("if ! config_after_update; then", "if ! service_start_after_update || ! service_is_active; then")
order("if ! restore_update_database; then", "service_start_after_update >/dev/null 2>&1 || true")

# Migration/configuration failures must escape config_after_update instead of
# being printed and ignored, and helper-driven SSL paths must not start the
# candidate while the transaction is deferred.
require('if ! ${xui_folder}/x-ui migrate; then')
require('return 1\n    fi\n\n    # Properly detect empty cert')
require('update_defer_service_start=1')
require('service_start_for_config()')
require('service_restart_for_config()')

print("update transaction static checks passed")
PY

python3 - "$script" "${root_dir}/install-phoenix.sh" <<'PY'
from pathlib import Path
import sys

for raw in sys.argv[1:]:
    text = Path(raw).read_text()
    name = Path(raw).name
    if "apt_get_locked() {" not in text:
        raise SystemExit(f"{name}: missing apt lock helper")
    if "apt-get -o DPkg::Lock::Timeout=300" not in text:
        raise SystemExit(f"{name}: apt lock helper must wait for the dpkg frontend lock")
    for number, line in enumerate(text.splitlines(), 1):
        if "apt-get " in line and "DPkg::Lock::Timeout" not in line:
            raise SystemExit(f"{name}:{number}: raw apt-get bypasses the lock helper")
        if line.lstrip().startswith("apt install"):
            raise SystemExit(f"{name}:{number}: raw apt bypasses the lock helper")
print("apt lock static checks passed")
PY

python3 - "${root_dir}/install-phoenix.sh" "${root_dir}/install.sh" <<'PY'
from pathlib import Path
import sys

for raw in sys.argv[1:]:
    text = Path(raw).read_text()
    name = Path(raw).name
    require = lambda needle: (_ for _ in ()).throw(SystemExit(f"{name}: missing runtime prerequisite guard: {needle}")) if needle not in text else None
    require("verify_runtime_prerequisites() {")
    require("install_base\nif ! verify_runtime_prerequisites; then")
    require("local required_commands=(curl tar unzip openssl socat crontab ip ss sysctl modprobe sha256sum mktemp find grep sed awk sudo)")
    require("command -v psql")
    require("command -v pg_dump")
    require("command -v pg_restore")
    require("ensure_pg_client || {")
    # The verification pass must happen before any daemon-specific installer.
    if text.index("if ! verify_runtime_prerequisites; then") >= text.index("if ! install_openvpn; then"):
        raise SystemExit(f"{name}: prerequisite check must run before daemon installers")
print("installer runtime prerequisite static checks passed")
PY

python3 - "${root_dir}/update.sh" <<'PY'
from pathlib import Path
import sys

text = Path(sys.argv[1]).read_text()
for needle in (
    "verify_runtime_prerequisites() {",
    "install_base\nif ! verify_runtime_prerequisites; then",
    "ensure_pg_client() {",
    "command -v psql",
    "command -v pg_dump",
    "command -v pg_restore",
):
    if needle not in text:
        raise SystemExit(f"update.sh: missing runtime prerequisite guard: {needle}")
if text.index("if ! verify_runtime_prerequisites; then") >= text.index("if ! install_pppvpn; then"):
    raise SystemExit("update.sh: prerequisite check must run before daemon installers")
print("update runtime prerequisite static checks passed")
PY
