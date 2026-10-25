#!/usr/bin/env bash
set -euo pipefail

APP_DIR="/opt/phoenix-simurgh"
SIMURGH_HOME="/etc/phoenix-simurgh"
PHOENIX_REPO="${PHOENIX_REPO:-Dark-Sky07/PHOENIX}"
PHOENIX_REF="${PHOENIX_REF:-${PHOENIX_TAG:-}}"

say() { printf '%s\n' "[Simurgh] $*"; }

if [[ "${EUID}" -ne 0 ]]; then
  say "Run as root; skipping the optional service installation."
  exit 0
fi

FRESH_SETUP_PANEL=false
FRESH_SIMURGH_INSTALL=false
saved_role=""
if [[ -r "$SIMURGH_HOME/phoenix-role" ]]; then
  read -r saved_role < "$SIMURGH_HOME/phoenix-role" || true
fi
case "${saved_role,,}" in exit|relay) ;; *) saved_role="" ;; esac
if [[ ! -f "$SIMURGH_HOME/exit.toml" && ! -f "$SIMURGH_HOME/relay.toml" && -z "$saved_role" ]]; then
  FRESH_SIMURGH_INSTALL=true
fi

systemd_is_running() {
  command -v systemctl >/dev/null 2>&1 || return 1
  systemctl show --property=Version >/dev/null 2>&1 || return 1
  state="$(systemctl is-system-running 2>/dev/null || true)"
  [[ "$state" == running || "$state" == degraded || "$state" == maintenance ]]
}

simurgh_panel_port_in_use() {
  if command -v ss >/dev/null 2>&1; then
    ss -ltnH 2>/dev/null | awk '$4 ~ /:18787$/ { found=1 } END { exit !found }'
  else
    (exec 3<>/dev/tcp/127.0.0.1/18787) >/dev/null 2>&1
  fi
}

if [[ -n "${PHOENIX_SIMURGH_ROLE:-}" ]]; then
  case "${PHOENIX_SIMURGH_ROLE,,}" in
    exit|relay) PHOENIX_SIMURGH_ROLE="${PHOENIX_SIMURGH_ROLE,,}" ;;
    *) say "PHOENIX_SIMURGH_ROLE must be 'exit' or 'relay'."; exit 1 ;;
  esac
  export PHOENIX_SIMURGH_ROLE
else
  saved_role=""
  if [[ -r "$SIMURGH_HOME/phoenix-role" ]]; then
    read -r saved_role < "$SIMURGH_HOME/phoenix-role" || true
  fi
  case "${saved_role,,}" in exit|relay) ;; *) saved_role="" ;; esac
  if [[ ! -f "$SIMURGH_HOME/exit.toml" && ! -f "$SIMURGH_HOME/relay.toml" && -z "$saved_role" ]]; then
    if systemd_is_running; then
      if systemctl is-active --quiet phoenix-simurgh-setup.service \
          && [[ -f /etc/systemd/system/phoenix-simurgh-setup.service ]] \
          && grep -q 'PHOENIX bundled Simurgh panel' /etc/systemd/system/phoenix-simurgh-setup.service; then
        say "The authenticated Simurgh first-run panel is already running on loopback port 18787."
        say "Use an SSH tunnel: ssh -L 18787:127.0.0.1:18787 <user>@<server>, then open http://127.0.0.1:18787/login."
        exit 0
      fi
      unit_conflict=false
      for unit in simurgh-setup simurgh-exit simurgh-relay; do
        load_state="$(systemctl show --property=LoadState --value "$unit" 2>/dev/null || true)"
        load_state="${load_state,,}"
        load_state="${load_state#loadstate=}"
        if [[ -n "$load_state" && "$load_state" != not-found && "$load_state" != *"could not be found"* ]]; then
          unit_conflict=true
          break
        fi
      done
      for phoenix_unit in \
          /etc/systemd/system/phoenix-simurgh-setup.service \
          /etc/systemd/system/phoenix-simurgh-exit.service \
          /etc/systemd/system/phoenix-simurgh-relay.service; do
        if [[ -e "$phoenix_unit" ]] && ! grep -q 'PHOENIX bundled Simurgh panel' "$phoenix_unit"; then
          unit_conflict=true
          break
        fi
      done
      for role in exit relay; do
        if systemctl is-active --quiet "phoenix-simurgh-${role}.service"; then
          unit_conflict=true
          break
        fi
      done
      if [[ "$unit_conflict" == true ]]; then
        say "An existing Simurgh systemd unit was detected; leaving it untouched and refusing to create a competing first-run service."
        exit 1
      fi
      if simurgh_panel_port_in_use; then
        say "Local port 18787 is already in use; no Simurgh setup service or config was changed."
        exit 1
      fi
      FRESH_SETUP_PANEL=true
    elif [[ -r /dev/tty && -w /dev/tty ]]; then
      printf '%s\n' "Choose the role for this Simurgh server:" \
        "  1) Foreign / exit — this server hosts the destination services" \
        "  2) Iran / relay — users connect to this server" > /dev/tty
      read -r -p "Select 1 or 2: " ROLE_CHOICE </dev/tty || ROLE_CHOICE=""
      case "${ROLE_CHOICE,,}" in
        1|exit|foreign) PHOENIX_SIMURGH_ROLE="exit" ;;
        2|relay|iran) PHOENIX_SIMURGH_ROLE="relay" ;;
        *) say "No valid role was selected; Simurgh was not configured."; exit 1 ;;
      esac
      export PHOENIX_SIMURGH_ROLE
    else
      say "A fresh Simurgh install needs a role. Run this interactively or set PHOENIX_SIMURGH_ROLE=exit|relay; no Simurgh files or configuration were changed."
      exit 1
    fi
  fi
fi

install_python_if_missing() {
  command -v python3 >/dev/null 2>&1 && return 0
  . /etc/os-release 2>/dev/null || true
  case "${ID:-}" in
    debian|ubuntu|armbian) apt-get update && apt-get install -y python3 ;;
    fedora|rhel|rocky|almalinux|amzn|ol) dnf install -y python3 2>/dev/null || yum install -y python3 ;;
    centos) yum install -y python3 ;;
    arch|manjaro) pacman -Sy --noconfirm python ;;
    opensuse*|sles) zypper --non-interactive install python3 ;;
    alpine) apk add --no-cache python3 ;;
    *) return 1 ;;
  esac
}

if ! command -v python3 >/dev/null 2>&1 && ! install_python_if_missing; then
  say "Python could not be installed; PHOENIX will continue without the separate service."
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import sys; raise SystemExit(sys.version_info < (3, 9))'; then
  say "Python 3.9 or newer is required; the PHOENIX installer will continue without the service."
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
if [[ -z "$PHOENIX_REF" ]]; then
  PHOENIX_REF="$(curl -4fsSL "https://api.github.com/repos/${PHOENIX_REPO}/releases/latest" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tag_name", ""))' 2>/dev/null || true)"
fi
if [[ -z "$PHOENIX_REF" ]]; then
  say "Could not resolve the PHOENIX release containing the verified Simurgh installer."
  exit 1
fi

mkdir -p "$APP_DIR/lib" "$SIMURGH_HOME"
chmod 700 "$SIMURGH_HOME"

# A fresh install uses the latest stable, SHA-256-verified upstream package.
# Existing installations are left in place here and updated transactionally by
# the PHOENIX updater after this service is healthy.
if [[ ! -f "$APP_DIR/lib/simurgh/__init__.py" || "$FRESH_SIMURGH_INSTALL" == true ]]; then
  for helper in update_simurgh.py build_latest_simurgh.py simurgh-role.py; do
    if ! curl -fL --retry 2 --connect-timeout 10 \
      "https://raw.githubusercontent.com/${PHOENIX_REPO}/${PHOENIX_REF}/scripts/${helper}" \
      -o "$work/${helper}"; then
      say "Could not download ${helper} from PHOENIX ${PHOENIX_REF}; Simurgh was not installed."
      exit 1
    fi
  done
  if ! python3 -m py_compile "$work/update_simurgh.py" "$work/build_latest_simurgh.py" "$work/simurgh-role.py"; then
    say "The downloaded Simurgh installer helpers failed validation."
    exit 1
  fi
  if ! python3 "$work/build_latest_simurgh.py" "$APP_DIR/lib"; then
    say "Could not install the latest verified Simurgh package; PHOENIX remains available."
    exit 1
  fi
fi

if [[ ! -s "$work/simurgh-role.py" ]] && ! curl -fL --retry 2 --connect-timeout 10 \
  "https://raw.githubusercontent.com/${PHOENIX_REPO}/${PHOENIX_REF}/scripts/simurgh-role.py" \
  -o "$work/simurgh-role.py"; then
  say "Could not download the Simurgh role detector from PHOENIX ${PHOENIX_REF}."
  exit 1
fi
if ! python3 -m py_compile "$work/simurgh-role.py"; then
  say "The Simurgh role detector failed validation."
  exit 1
fi
install -m 644 "$work/simurgh-role.py" "$APP_DIR/simurgh-role.py"

if [[ ! -f "$APP_DIR/lib/simurgh/__init__.py" ]] || \
   ! PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}" python3 -m simurgh --version >/dev/null 2>&1; then
  say "The Simurgh package is missing or failed its version smoke test."
  exit 1
fi
install -m 644 "$(dirname "$0")/phoenix-simurgh-launcher.py" "$APP_DIR/launcher.py"
cat > "$APP_DIR/simurgh" <<'SH'
#!/bin/sh
APP_DIR="/opt/phoenix-simurgh"
export PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}"
exec python3 "$APP_DIR/launcher.py" "$@"
SH
chmod 755 "$APP_DIR/simurgh"
chmod 644 "$APP_DIR/launcher.py"

cleanup_phoenix_setup_service() {
  setup_unit="/etc/systemd/system/phoenix-simurgh-setup.service"
  alias_unit="/etc/systemd/system/simurgh-setup.service"
  alias_target=""
  if [[ -L "$alias_unit" ]]; then
    alias_target="$(readlink -f "$alias_unit" 2>/dev/null || true)"
  fi
  if [[ -f "$setup_unit" ]] && grep -q 'PHOENIX bundled Simurgh panel' "$setup_unit"; then
    systemctl disable --now phoenix-simurgh-setup.service >/dev/null 2>&1 || true
    rm -f "$setup_unit"
    if [[ "$alias_target" == "$setup_unit" ]]; then
      rm -f "$alias_unit"
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
}

if [[ "$FRESH_SETUP_PANEL" == true ]]; then
  if ! SIMURGH_HOME="$SIMURGH_HOME" "$APP_DIR/simurgh" install >"$work/setup.log" 2>&1; then
    cat "$work/setup.log"
    cleanup_phoenix_setup_service
    say "The authenticated first-run panel could not be installed; no role config was created. PHOENIX and Xray were left untouched."
    exit 1
  fi
  if ! systemctl is-active --quiet phoenix-simurgh-setup.service; then
    cat "$work/setup.log"
    cleanup_phoenix_setup_service
    say "The first-run panel service did not become active; no role config was created. Inspect journalctl -u phoenix-simurgh-setup.service."
    exit 1
  fi
  if ! SIMURGH_HOME="$SIMURGH_HOME" PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}" python3 - <<'PY'
import base64
import json
import time
import urllib.error
import urllib.request
from pathlib import Path

base = "http://127.0.0.1:18787"
state_path = Path("/etc/phoenix-simurgh/state.json")
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
deadline = time.monotonic() + 30
last_error = "no response"
while time.monotonic() < deadline:
    try:
        with opener.open(base + "/healthz", timeout=2) as response:
            health = json.load(response)
        if (health.get("ok") is not True or health.get("product") != "Simurgh Tunnel"
                or health.get("role") != "setup"):
            raise RuntimeError("setup health endpoint returned unexpected metadata")
        state = json.loads(state_path.read_text(encoding="utf-8"))
        auth = state.get("panel_auth") or {}
        user, password = auth.get("user"), auth.get("password")
        if not user or not password:
            raise RuntimeError("first-run panel credentials were not initialized")
        token = base64.b64encode(f"{user}:{password}".encode()).decode()
        request = urllib.request.Request(
            base + "/", headers={"Authorization": "Basic " + token})
        with opener.open(request, timeout=3) as response:
            html = response.read(256 * 1024).decode("utf-8", "replace")
        if 'value="exit"' not in html or 'value="relay"' not in html:
            raise RuntimeError("authenticated first-run role chooser is incomplete")
        print(f"Simurgh first-run panel verified: v{health.get('version')} (authenticated role chooser OK).")
        raise SystemExit(0)
    except (OSError, ValueError, KeyError, RuntimeError, urllib.error.URLError) as exc:
        last_error = str(exc)
        time.sleep(0.5)
print(f"Simurgh first-run panel check failed: {last_error}")
raise SystemExit(1)
PY
  then
    cleanup_phoenix_setup_service
    say "The first-run panel failed its authenticated health check; its PHOENIX service was stopped. No role config was created."
    exit 1
  fi
  cat "$work/setup.log"
  say "Choose Exit or Relay in the authenticated Simurgh panel. It is loopback-only; from your computer run ssh -L 18787:127.0.0.1:18787 <user>@<server>, then open http://127.0.0.1:18787/login."
  say "The Simurgh role service starts only after you choose. PHOENIX and Xray remain under their existing service managers."
  exit 0
fi

role_info="$(SIMURGH_HOME="$SIMURGH_HOME" PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}" \
  python3 "$APP_DIR/simurgh-role.py" --home "$SIMURGH_HOME" --json)" || {
  say "Could not determine the Simurgh server role."
  exit 1
}
SIMURGH_ROLE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["role"])' <<<"$role_info")"
MIGRATE_RELAY="$(python3 -c 'import json,sys; print(str(json.load(sys.stdin)["migrate_relay"]).lower())' <<<"$role_info")"
if [[ -z "$SIMURGH_ROLE" ]]; then
  say "Could not determine a safe Simurgh role; no Simurgh service was changed."
  exit 1
fi
PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE"
export PHOENIX_SIMURGH_ROLE
say "Selected or retained Simurgh role: $SIMURGH_ROLE."
SERVICE_NAME="phoenix-simurgh-${SIMURGH_ROLE}"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
CONFIG_FILE="$SIMURGH_HOME/${SIMURGH_ROLE}.toml"
CONFIG_CREATED=false

verify_panel() {
  SIMURGH_HOME="$SIMURGH_HOME" PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE" \
    PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}" \
    python3 - <<'PY'
import base64
import json
import os
import time
import urllib.error
import urllib.request
from pathlib import Path

base = "http://127.0.0.1:18787"
state_path = Path(os.environ["SIMURGH_HOME"]) / "state.json"
expected_role = os.environ["PHOENIX_SIMURGH_ROLE"]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
deadline = time.monotonic() + 30
last_error = "no response"
while time.monotonic() < deadline:
    try:
        with opener.open(base + "/healthz", timeout=2) as response:
            health = json.load(response)
        if (health.get("ok") is not True or health.get("product") != "Simurgh Tunnel"
                or health.get("role") != expected_role):
            raise RuntimeError("health endpoint returned unexpected service metadata")

        state = json.loads(state_path.read_text(encoding="utf-8"))
        auth = state.get("panel_auth") or {}
        user, password = auth.get("user"), auth.get("password")
        if not user or not password:
            raise RuntimeError("panel credentials were not initialized")
        token = base64.b64encode(f"{user}:{password}".encode()).decode()
        request = urllib.request.Request(base + "/", headers={"Authorization": "Basic " + token})
        with opener.open(request, timeout=3) as response:
            html = response.read(256 * 1024).decode("utf-8", "replace")
        if "<title>Simurgh Tunnel</title>" not in html or 'src="/app.js"' not in html:
            raise RuntimeError("panel HTML is missing its expected title or JavaScript bundle")
        for asset in ("/app.js", "/app.css"):
            with opener.open(base + asset, timeout=3) as response:
                if response.status != 200 or not response.read(64):
                    raise RuntimeError(f"panel asset failed to load: {asset}")
        print(f"Simurgh panel verified: v{health.get('version')} (health, login, and assets OK).")
        raise SystemExit(0)
    except (OSError, ValueError, KeyError, RuntimeError, urllib.error.URLError) as exc:
        last_error = str(exc)
        time.sleep(0.5)
print(f"Simurgh panel health/UI check failed: {last_error}")
raise SystemExit(1)
PY
}

if [[ ! -s "$CONFIG_FILE" ]]; then
  if [[ "$SIMURGH_ROLE" == "relay" ]]; then
    if ! SIMURGH_HOME="$SIMURGH_HOME" "$APP_DIR/simurgh" install \
      --role relay --pair-later --panel-port 18787 --engine python \
      --no-units --no-enable --no-start --force >"$work/configure.log" 2>&1; then
      say "Could not create the idle relay configuration; PHOENIX and Xray are untouched."
      exit 1
    fi
  else
    if ! SIMURGH_HOME="$SIMURGH_HOME" "$APP_DIR/simurgh" install \
      --role exit --panel-port 18787 --engine python \
      --no-units --no-enable --no-start --force >"$work/configure.log" 2>&1; then
      say "Could not create the foreign-server configuration; PHOENIX and Xray are untouched."
      exit 1
    fi
    # Keep the automatic foreign-role install panel-only until an operator
    # chooses tunnel listeners. This avoids binding ports already used by Xray.
    if ! SIMURGH_HOME="$SIMURGH_HOME" PYTHONPATH="$APP_DIR/lib${PYTHONPATH:+:$PYTHONPATH}" \
      python3 - "$CONFIG_FILE" <<'PY'
import sys
from simurgh.config import load_exit, save_exit
path = sys.argv[1]
config = load_exit(path)
for listener in config.listen:
    listener.enabled = False
config.push_ports = []
config.push_enabled = False
save_exit(config, path)
PY
    then
      say "Could not leave the foreign-server config in safe panel-only mode."
      exit 1
    fi
  fi
  CONFIG_CREATED=true
fi

discard_staged_exit() {
  if [[ "$MIGRATE_RELAY" == "true" && "$CONFIG_CREATED" == true && "$SIMURGH_ROLE" == "exit" ]]; then
    rm -f "$CONFIG_FILE"
  fi
}

if [[ "$MIGRATE_RELAY" == "true" ]]; then
  old_systemd_stopped=false
  old_openrc_stopped=false
  old_unit="/etc/systemd/system/phoenix-simurgh-relay.service"
  if command -v systemctl >/dev/null 2>&1 && systemctl show --property=Version >/dev/null 2>&1 \
      && systemctl is-active --quiet phoenix-simurgh-relay.service; then
    if [[ ! -f "$old_unit" ]] || ! grep -q 'PHOENIX bundled Simurgh panel' "$old_unit"; then
      say "The existing relay service is not managed by PHOENIX; refusing to migrate its config."
      discard_staged_exit
      exit 1
    fi
    if ! systemctl stop phoenix-simurgh-relay.service; then
      say "Could not safely stop the old idle relay service; no configuration was moved."
      systemctl start phoenix-simurgh-relay.service >/dev/null 2>&1 || true
      discard_staged_exit
      exit 1
    fi
    old_systemd_stopped=true
  elif command -v rc-service >/dev/null 2>&1 && rc-service phoenix-simurgh status >/dev/null 2>&1; then
    old_openrc="/etc/init.d/phoenix-simurgh"
    if [[ ! -f "$old_openrc" ]] || ! grep -q 'PHOENIX bundled Simurgh panel' "$old_openrc"; then
      say "The existing relay service is not managed by PHOENIX; refusing to migrate its config."
      discard_staged_exit
      exit 1
    fi
    if ! rc-service phoenix-simurgh stop; then
      say "Could not safely stop the old idle relay service; no configuration was moved."
      rc-service phoenix-simurgh start >/dev/null 2>&1 || true
      discard_staged_exit
      exit 1
    fi
    old_openrc_stopped=true
  fi
  if ! python3 - <<'PY' >/dev/null 2>&1
import socket
s = socket.socket()
s.bind(("127.0.0.1", 18787))
s.close()
PY
  then
    if [[ "$old_systemd_stopped" == true ]]; then
      systemctl start phoenix-simurgh-relay.service >/dev/null 2>&1 || say "Could not restart the previous relay service."
    elif [[ "$old_openrc_stopped" == true ]]; then
      rc-service phoenix-simurgh start >/dev/null 2>&1 || say "Could not restart the previous relay service."
    fi
    say "Local port 18787 is still occupied; preserving the old relay config and service."
    discard_staged_exit
    exit 1
  fi
  backup_dir="$SIMURGH_HOME/phoenix-backup"
  mkdir -p "$backup_dir"
  chmod 700 "$backup_dir"
  backup_file="$backup_dir/relay.toml.before-role-migration.$(date +%s)"
  if ! mv "$SIMURGH_HOME/relay.toml" "$backup_file"; then
    say "Could not preserve the previous relay config; it was not removed."
    if [[ "$old_systemd_stopped" == true ]]; then
      systemctl start phoenix-simurgh-relay.service >/dev/null 2>&1 || true
    elif [[ "$old_openrc_stopped" == true ]]; then
      rc-service phoenix-simurgh start >/dev/null 2>&1 || true
    fi
    discard_staged_exit
    exit 1
  fi
  if [[ -f "$old_unit" ]] && grep -q 'PHOENIX bundled Simurgh panel' "$old_unit"; then
    if command -v systemctl >/dev/null 2>&1; then
      systemctl disable phoenix-simurgh-relay.service >/dev/null 2>&1 || true
    fi
    rm -f "$old_unit"
    if command -v systemctl >/dev/null 2>&1; then
      systemctl daemon-reload || true
    fi
  fi
  if [[ -f /etc/init.d/phoenix-simurgh ]] && grep -q 'PHOENIX bundled Simurgh panel' /etc/init.d/phoenix-simurgh; then
    rc-update del phoenix-simurgh default >/dev/null 2>&1 || true
    rm -f /etc/init.d/phoenix-simurgh
  fi
  say "Moved the old unpaired relay config to $backup_file; detected role is exit."
fi

printf '%s\n' "$SIMURGH_ROLE" > "$SIMURGH_HOME/phoenix-role.tmp"
chmod 600 "$SIMURGH_HOME/phoenix-role.tmp"
mv -f "$SIMURGH_HOME/phoenix-role.tmp" "$SIMURGH_HOME/phoenix-role"

if ! python3 - <<'PY' >/dev/null 2>&1
import socket
s = socket.socket()
s.bind(("127.0.0.1", 18787))
s.close()
PY
then
  own_service_active=false
  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "$SERVICE_NAME.service"; then
    own_service_active=true
  elif command -v rc-service >/dev/null 2>&1 && rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
    own_service_active=true
  fi
  if [[ "$own_service_active" != true ]]; then
    say "Local port 18787 is already in use; leaving the service stopped to avoid interfering with another process."
    exit 1
  fi
fi

if command -v systemctl >/dev/null 2>&1 && systemctl show --property=Version >/dev/null 2>&1; then
  if [[ -e "$UNIT_FILE" ]] && ! grep -q 'PHOENIX bundled Simurgh panel' "$UNIT_FILE"; then
    say "An unrelated ${SERVICE_NAME}.service already exists; leaving it untouched."
    exit 1
  fi
  unit_temp="$UNIT_FILE.tmp"
  cat > "$unit_temp" <<UNIT
[Unit]
Description=PHOENIX bundled Simurgh panel
After=network-online.target x-ui.service
Wants=network-online.target

[Service]
Type=simple
Environment=SIMURGH_HOME=$SIMURGH_HOME
Environment=PHOENIX_SIMURGH_ROLE=$SIMURGH_ROLE
Environment=PYTHONUNBUFFERED=1
ExecStart=$APP_DIR/simurgh $SIMURGH_ROLE --panel-host 127.0.0.1 --panel-port 18787
Restart=on-failure
RestartSec=4
LimitNOFILE=1048576
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
  UNIT_CHANGED=false
  if ! cmp -s "$unit_temp" "$UNIT_FILE"; then
    mv -f "$unit_temp" "$UNIT_FILE"
    UNIT_CHANGED=true
    chmod 644 "$UNIT_FILE"
    systemctl daemon-reload
  else
    rm -f "$unit_temp"
  fi
  systemctl enable "$SERVICE_NAME.service" >/dev/null 2>&1 || true
  service_action=start
  if systemctl is-active --quiet "$SERVICE_NAME.service"; then
    if [[ "$UNIT_CHANGED" == true ]]; then
      service_action=restart
    else
      service_action=""
    fi
  fi
  if [[ -n "$service_action" ]] && ! systemctl "$service_action" "$SERVICE_NAME.service" >/dev/null 2>&1; then
    say "The Simurgh service failed to start; inspect: journalctl -u ${SERVICE_NAME}.service"
    exit 1
  fi
elif command -v rc-update >/dev/null 2>&1 && [[ -d /etc/init.d ]]; then
  openrc_unit="/etc/init.d/$SERVICE_NAME"
  if [[ -e "$openrc_unit" ]] && ! grep -q 'PHOENIX bundled Simurgh panel' "$openrc_unit"; then
    say "An unrelated $SERVICE_NAME OpenRC service already exists; leaving it untouched."
    exit 1
  fi
  openrc_temp="$openrc_unit.tmp"
  cat > "$openrc_temp" <<OPENRC
#!/sbin/openrc-run
description="PHOENIX bundled Simurgh panel"
command="$APP_DIR/simurgh"
command_args="$SIMURGH_ROLE --panel-host 127.0.0.1 --panel-port 18787"
command_background=true
pidfile="/run/$SERVICE_NAME.pid"
export SIMURGH_HOME="$SIMURGH_HOME"
export PHOENIX_SIMURGH_ROLE="$SIMURGH_ROLE"
export PYTHONUNBUFFERED="1"
OPENRC
  UNIT_CHANGED=false
  if ! cmp -s "$openrc_temp" "$openrc_unit"; then
    mv -f "$openrc_temp" "$openrc_unit"
    UNIT_CHANGED=true
    chmod 755 "$openrc_unit"
  else
    rm -f "$openrc_temp"
  fi
  rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 || true
  service_action=start
  if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
    if [[ "$UNIT_CHANGED" == true ]]; then
      service_action=restart
    else
      service_action=""
    fi
  fi
  if [[ -n "$service_action" ]] && ! rc-service "$SERVICE_NAME" "$service_action" >/dev/null 2>&1; then
    say "The Simurgh service failed to start; inspect the OpenRC service log."
    exit 1
  fi
else
  say "No supported service manager found; Simurgh was not started."
  exit 1
fi

if ! verify_panel; then
  say "Simurgh did not pass its health and UI checks; PHOENIX and Xray were left running."
  say "Inspect: journalctl -u ${SERVICE_NAME}.service"
  exit 1
fi

say "Simurgh Tunnel is installed and verified as role '$SIMURGH_ROLE'; the foreign-role tunnel listeners remain unbound until configured."
