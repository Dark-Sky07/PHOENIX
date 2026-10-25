import os
import shutil
import subprocess
from pathlib import Path

from simurgh import cli, menu, systemd


APP_DIR = Path("/opt/phoenix-simurgh")
SIMURGH_HOME = Path(os.environ.get("SIMURGH_HOME", "/etc/phoenix-simurgh"))
PANEL_PORT = 18787
UNIT_MARKER = "PHOENIX bundled Simurgh panel"
_SYSTEMD_UNIT_NAMES = {"simurgh-exit", "simurgh-relay", "simurgh-setup"}


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

# v3.2.13's first-run role chooser calls cmd_init directly. Keep its initial
# exit config panel-only, as PHOENIX/Xray may already own the usual tunnel ports.
_original_cmd_init = getattr(cli, "cmd_init", None)
if callable(_original_cmd_init):
    def _phoenix_cmd_init(args):
        result = _original_cmd_init(args)
        if result == 0 and getattr(args, "role", "") == "exit":
            try:
                from simurgh.config import load_exit, save_exit

                home = Path(getattr(args, "home", "") or os.environ.get(
                    "SIMURGH_HOME", "/etc/phoenix-simurgh"))
                cfg_path = home / "exit.toml"
                config = load_exit(cfg_path)
                for listener in config.listen:
                    listener.enabled = False
                config.push_ports = []
                config.push_enabled = False
                save_exit(config, cfg_path)
            except Exception as exc:
                print(f"Could not keep the new exit config panel-only: {exc}")
                return 1
        return result

    cli.cmd_init = _phoenix_cmd_init

_original_cmd_install = getattr(cli, "cmd_install", None)
if callable(_original_cmd_install):
    def _phoenix_cmd_install(args):
        if getattr(args, "both", False):
            print("PHOENIX manages exactly one Simurgh role per server; choose Exit or Relay.")
            return 2
        role = getattr(args, "role", "auto")
        if role in {"exit", "relay"} and not getattr(args, "no_units", False):
            home = Path(getattr(args, "home", "") or os.environ.get(
                "SIMURGH_HOME", "/etc/phoenix-simurgh"))
            if not (home / f"{role}.toml").is_file():
                print("For a fresh PHOENIX setup, omit --role to use the authenticated role chooser, or run simurgh menu. No config was changed.")
                return 2
        return _original_cmd_install(args)

    cli.cmd_install = _phoenix_cmd_install

# The PHOENIX Simurgh panel is loopback-only; operators reach it via SSH
# forwarding. Keep upstream's first-run URL consistent with that boundary.
if not (SIMURGH_HOME / "exit.toml").exists() and not (SIMURGH_HOME / "relay.toml").exists():
    cli.RELAY_DEFAULT_PANEL = PANEL_PORT
    if hasattr(cli, "_public_ip"):
        cli._public_ip = lambda: "127.0.0.1"


def running_in_docker():
    return os.environ.get("XUI_IN_DOCKER", "").lower() == "true" or os.path.exists("/.dockerenv")


_OPENRC = not systemd.available() and bool(shutil.which("rc-service") and shutil.which("rc-update"))


def _phoenix_unit_path(service):
    if service in _SYSTEMD_UNIT_NAMES:
        return systemd.UNIT_DIR / f"phoenix-{service}.service"
    return _original_unit_path(service)


def _phoenix_render_unit(home, role, name="", user=None):
    if role not in {"exit", "relay", "setup"}:
        return _original_render_unit(home, role, name=name, user=user)
    user_line = f"User={user}\n" if user else ""
    if role == "setup":
        exec_start = f"{APP_DIR}/simurgh setup --panel-host 127.0.0.1 --panel-port {PANEL_PORT}"
        log_name = "panel.log"
        environment = ""
    else:
        exec_start = f"{APP_DIR}/simurgh {role} --panel-host 127.0.0.1 --panel-port {PANEL_PORT}"
        log_name = f"{role}.log"
        environment = f"Environment=PHOENIX_SIMURGH_ROLE={role}\n"
    title = f" - {name}" if name else ""
    alias = f"Alias=simurgh-{role}.service\n"
    return (
        "[Unit]\n"
        f"Description={UNIT_MARKER}{title} ({role})\n"
        "Documentation=https://github.com/Dark-Sky07/Simurgh-Tunnel\n"
        "After=network-online.target\n"
        "Wants=network-online.target\n\n"
        "[Service]\nType=simple\n"
        f"{user_line}Environment=SIMURGH_HOME={home.path}\n"
        f"Environment=PYTHONPATH={APP_DIR}/lib\n"
        f"{environment}Environment=PYTHONUNBUFFERED=1\n"
        f"ExecStart={exec_start}\n"
        "Restart=on-failure\nRestartSec=4\n"
        "LimitNOFILE=1048576\nNoNewPrivileges=true\n"
        f"StandardOutput=append:{home.logs / log_name}\n"
        f"StandardError=append:{home.logs / log_name}\n\n"
        "[Install]\nWantedBy=multi-user.target\n"
        f"{alias}"
    )


def _write_role_marker(home, role):
    if role not in {"exit", "relay"}:
        return
    config = home.exit_cfg if role == "exit" else home.relay_cfg
    if not config.is_file():
        return
    marker = home.path / "phoenix-role"
    temporary = marker.with_suffix(".tmp")
    temporary.write_text(role + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    os.replace(temporary, marker)


def _alias_is_available(service):
    """Do not claim a generic upstream unit name already owned by another app."""
    alias_path = systemd.UNIT_DIR / f"{service}.service"
    target_path = systemd.UNIT_DIR / f"phoenix-{service}.service"
    try:
        if alias_path.exists() or alias_path.is_symlink():
            return alias_path.is_symlink() and alias_path.resolve() == target_path.resolve()
    except OSError:
        return False
    run = getattr(systemd, "_run", None)
    if not callable(run):
        return False
    try:
        code, output = run([
            "systemctl", "show", service, "--property=LoadState", "--value",
        ], timeout=15)
    except Exception:
        return False
    state = output.strip().lower().replace("loadstate=", "")
    if state == "not-found" or "could not be found" in state:
        return True
    return code == 0 and not state


def _phoenix_systemd_control(action, services=None):
    if services is None:
        services = getattr(systemd, "SERVICES", ("simurgh-exit", "simurgh-relay"))
    if _OPENRC:
        return _openrc_control(action, services)
    results = {}
    for service in services:
        original = service
        target = service
        if service in _SYSTEMD_UNIT_NAMES:
            if action == "enable" and not _alias_is_available(service):
                results[original] = "refused: the generic Simurgh unit name is already owned"
                continue
            target = f"phoenix-{service}"
        report = _original_control(action, (target,))
        results[original] = report.get(target, next(iter(report.values()), "unknown systemd result"))
    return results


def _unit_is_phoenix_owned(path):
    if not path.exists():
        return True
    try:
        with path.open("r", encoding="utf-8", errors="replace") as unit_file:
            return UNIT_MARKER in unit_file.read()
    except OSError:
        return False


def _phoenix_install_services(home, roles=("exit", "relay"), enable=True,
                              start=True, user=None, write=True):
    roles = tuple(roles)
    if len(roles) != 1 or roles[0] not in {"exit", "relay"}:
        return {"systemd": systemd.available(), "units": [], "actions": {}, "write": False,
                "errors": {"roles": "PHOENIX manages exactly one Simurgh role per server"}}
    if _OPENRC:
        return {"systemd": False, "units": [], "actions": {}, "write": write}
    if not write or not systemd.available():
        return _original_install_services(
            home, roles=roles, enable=enable, start=start, user=user, write=write)
    if write:
        for role in roles:
            path = _phoenix_unit_path(f"simurgh-{role}")
            if not _unit_is_phoenix_owned(path):
                return {"systemd": True, "units": [], "actions": {}, "write": False,
                        "errors": {str(path): "refusing to overwrite a unit not managed by PHOENIX"}}
    if enable and any(not _alias_is_available(f"simurgh-{role}") for role in roles):
        return {"systemd": True, "units": [], "actions": {}, "write": False,
                "errors": {"systemd alias": "a generic Simurgh unit name is already owned"}}
    result = _original_install_services(
        home, roles=roles, enable=enable, start=start, user=user, write=write)
    if (result.get("systemd") and len(roles) == 1
            and not result.get("errors") and result.get("write", True)):
        _write_role_marker(home, roles[0])
    return result


def _phoenix_install_setup_service(home, user=None, enable=True, start=True, write=True):
    if _OPENRC:
        return {"systemd": False, "units": [], "actions": {}, "write": write}
    if not write or not systemd.available():
        return _original_install_setup_service(
            home, user=user, enable=enable, start=start, write=write)
    path = _phoenix_unit_path("simurgh-setup")
    if write and not _unit_is_phoenix_owned(path):
        return {"systemd": True, "units": [], "actions": {}, "write": False,
                "errors": {str(path): "refusing to overwrite a unit not managed by PHOENIX"}}
    if enable and not _alias_is_available("simurgh-setup"):
        return {"systemd": True, "units": [], "actions": {}, "write": False,
                "errors": {"systemd alias": "the generic setup unit name is already owned"}}
    return _original_install_setup_service(
        home, user=user, enable=enable, start=start, write=write)


# v3.2.13's setup panel hands off through simurgh-{setup,exit,relay}. Keep
# those names as checked systemd aliases while the actual units remain owned
# and named by PHOENIX. Never remove a PHOENIX unit through Simurgh's CLI.
_original_unit_path = getattr(systemd, "unit_path", None)
_original_render_unit = getattr(systemd, "render_unit", None)
_original_control = getattr(systemd, "control", None)
_original_install_services = getattr(systemd, "install_services", None)
_original_install_setup_service = getattr(systemd, "install_setup_service", None)
if (not _OPENRC and callable(_original_unit_path) and callable(_original_render_unit)
        and callable(_original_control) and callable(_original_install_services)
        and callable(_original_install_setup_service) and hasattr(systemd, "UNIT_DIR")):
    systemd.unit_path = _phoenix_unit_path
    systemd.render_unit = _phoenix_render_unit
    systemd.control = _phoenix_systemd_control
    systemd.install_services = _phoenix_install_services
    systemd.install_setup_service = _phoenix_install_setup_service
    if hasattr(systemd, "uninstall_services"):
        systemd.uninstall_services = lambda *args, **kwargs: {
            "actions": {"phoenix-simurgh": "refused: managed by the PHOENIX installer"}}

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

    def _openrc_install_services(home, roles=("exit", "relay"), enable=True,
                                start=True, user=None, write=True):
        return {"systemd": False, "units": [], "actions": {}, "write": False}

    def _openrc_install_setup_service(home, user=None, enable=True, start=True, write=True):
        return {"systemd": False, "units": [], "actions": {}, "write": False}

    systemd.install_services = _openrc_install_services
    if hasattr(systemd, "install_setup_service"):
        systemd.install_setup_service = _openrc_install_setup_service


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
