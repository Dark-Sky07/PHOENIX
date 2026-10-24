import os
import shutil
import subprocess
from pathlib import Path

from simurgh import cli, menu, systemd


def configured_role():
    override = os.environ.get("PHOENIX_SIMURGH_ROLE", "").strip().lower()
    if override in {"exit", "relay"}:
        return override
    home = Path(os.environ.get("SIMURGH_HOME", "/etc/phoenix-simurgh"))
    marker = home / "phoenix-role"
    if marker.is_file():
        saved = marker.read_text(encoding="utf-8").strip().lower()
        if saved in {"exit", "relay"}:
            return saved
    if (home / "exit.toml").is_file() and not (home / "relay.toml").is_file():
        return "exit"
    return "relay"


ROLE = configured_role()
SERVICE = f"phoenix-simurgh-{ROLE}"

# Keep role detection and service actions scoped to the config selected by PHOENIX.
_detect_role = getattr(cli, "_detect_role", None)
if _detect_role:
    def _phoenix_detect_role(home, role):
        if role in {"exit", "relay"}:
            return role
        return ROLE

    cli._detect_role = _phoenix_detect_role
cli._service_names = lambda role: (SERVICE,) if role == ROLE else ()


def running_in_docker():
    return os.environ.get("XUI_IN_DOCKER", "").lower() == "true" or os.path.exists("/.dockerenv")


_OPENRC = not systemd.available() and bool(shutil.which("rc-service") and shutil.which("rc-update"))

# OpenRC distributions do not have systemd's service manager. Route actions to
# the PHOENIX-owned role-specific unit, never to a generic upstream service.
if _OPENRC:
    systemd.available = lambda: True

    def _openrc_control(action, services):
        results = {}
        for service in services:
            if service != SERVICE:
                results[service] = "refused: not the bundled service"
                continue
            command = ["rc-service", SERVICE, action]
            if action == "enable":
                command = ["rc-update", "add", SERVICE, "default"]
            elif action == "disable":
                command = ["rc-update", "del", SERVICE, "default"]
            result = subprocess.run(command, capture_output=True, text=True, timeout=30)
            results[service] = "ok" if result.returncode == 0 else (result.stderr or result.stdout).strip()
        return results

    systemd.control = _openrc_control


def _bundled_service_state(service):
    if service != f"simurgh-{ROLE}":
        return "not managed by PHOENIX"
    if running_in_docker():
        return "managed by Docker"
    if _OPENRC:
        result = subprocess.run(
            ["rc-service", SERVICE, "status"],
            capture_output=True, text=True, timeout=15,
        )
        return "active" if result.returncode == 0 else "stopped"
    if not systemd.available():
        return "no systemd/OpenRC"
    state = systemd.service_state(SERVICE)
    active = state.get("ActiveState", "?")
    sub = state.get("SubState", "")
    label = {"active": menu.c("active", "green"),
             "inactive": menu.c("stopped", "red"),
             "failed": menu.c("failed", "red"),
             "activating": menu.c("starting", "yellow")}.get(active, active)
    return label + (f"/{sub}" if sub else "")


def _bundled_service_action(role, action):
    if role != ROLE:
        print(menu.c("\n  PHOENIX only manages the detected Simurgh role.", "red"))
        menu.pause()
        return
    if running_in_docker():
        print(menu.c("\n  Simurgh is managed by Docker; pull and recreate the PHOENIX image.", "yellow"))
        menu.pause()
        return
    if not systemd.available():
        print(menu.c("\n  No supported service manager is available.", "red"))
        menu.pause()
        return
    results = systemd.control(action, (SERVICE,))
    for name, result in results.items():
        mark = menu.c("✔", "green") if result == "ok" else menu.c("✘", "red")
        print(f"  {mark} {name}: {action} {'' if result == 'ok' else result}")
    menu.time.sleep(0.8)


menu.service_state = _bundled_service_state
menu.service_action = _bundled_service_action


def main(argv=None):
    return cli.main(argv)


if __name__ == "__main__":
    raise SystemExit(main())
