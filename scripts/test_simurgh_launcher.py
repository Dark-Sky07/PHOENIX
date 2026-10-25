#!/usr/bin/env python3
"""Verify the bundled Simurgh CLI menu cannot control another service."""
from __future__ import annotations

import importlib.util
import io
import os
import sys
import tempfile
import types
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

SCRIPT = Path(__file__).with_name("phoenix-simurgh-launcher.py")


def load_launcher(role="relay"):
    package = types.ModuleType("simurgh")
    package.__path__ = []
    cli = types.ModuleType("simurgh.cli")
    cli.main = Mock(return_value=0)
    cli.cmd_install = Mock(return_value=0)
    systemd = types.ModuleType("simurgh.systemd")
    systemd.available = Mock(return_value=True)
    systemd.UNIT_DIR = Path("/etc/systemd/system")
    systemd.SERVICES = ("simurgh-exit", "simurgh-relay")
    systemd.unit_path = lambda service: systemd.UNIT_DIR / f"{service}.service"
    systemd.render_unit = lambda home, role, name="", user=None: "upstream unit"
    systemd.control = Mock(return_value={"phoenix-simurgh-relay": "ok"})
    systemd.install_services = Mock(return_value={"systemd": True, "units": [], "actions": {}, "write": True})
    systemd.install_setup_service = Mock(return_value={"systemd": True, "units": [], "actions": {}, "write": True})
    systemd.uninstall_services = Mock(return_value={"actions": {}})
    systemd._run = Mock(return_value=(0, "not-found"))
    systemd.service_state = Mock(return_value={"ActiveState": "active", "SubState": "running"})
    menu = types.ModuleType("simurgh.menu")
    menu.service_state = Mock(return_value="upstream-service")
    menu.service_action = Mock()
    menu.c = lambda text, _colour: text
    menu.pause = Mock()
    menu.time = types.SimpleNamespace(sleep=Mock())
    package.cli, package.menu, package.systemd = cli, menu, systemd
    modules = {"simurgh": package, "simurgh.cli": cli,
               "simurgh.menu": menu, "simurgh.systemd": systemd}
    spec = importlib.util.spec_from_file_location("phoenix_simurgh_launcher_test", SCRIPT)
    assert spec and spec.loader
    launcher = importlib.util.module_from_spec(spec)
    with patch.dict(sys.modules, modules), patch.dict(os.environ, {"PHOENIX_SIMURGH_ROLE": role}):
        spec.loader.exec_module(launcher)
    return launcher, cli, menu, systemd


class FirstRunHandoffTests(unittest.TestCase):
    def test_unsafe_fresh_install_options_are_refused_before_upstream_configuration(self):
        launcher, cli, _menu, _systemd = load_launcher()
        self.assertEqual(cli.cmd_install(SimpleNamespace(both=True)), 2)
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(cli.cmd_install(SimpleNamespace(
                both=False, role="exit", no_units=False, home=directory)), 2)
        launcher._original_cmd_install.assert_not_called()

    def test_upstream_setup_units_are_redirected_to_loopback_phoenix_units(self):
        launcher, _cli, _menu, systemd = load_launcher()
        with tempfile.TemporaryDirectory() as directory:
            systemd.UNIT_DIR = Path(directory)
            home_path = Path(directory) / "home"
            log_path = home_path / "logs"
            home_path.mkdir()
            log_path.mkdir()
            home = SimpleNamespace(path=home_path, logs=log_path)

            self.assertEqual(systemd.unit_path("simurgh-setup"),
                             Path(directory) / "phoenix-simurgh-setup.service")
            setup = systemd.render_unit(home, "setup")
            self.assertIn("ExecStart=/opt/phoenix-simurgh/simurgh setup --panel-host 127.0.0.1 --panel-port 18787", setup)
            self.assertIn("Alias=simurgh-setup.service", setup)
            self.assertNotIn("0.0.0.0", setup)

            role = systemd.render_unit(home, "exit")
            self.assertIn("Environment=PHOENIX_SIMURGH_ROLE=exit", role)
            self.assertIn("Alias=simurgh-exit.service", role)
            self.assertIn("127.0.0.1 --panel-port 18787", role)

    def test_generic_unit_control_translates_only_when_alias_is_unclaimed(self):
        launcher, _cli, _menu, systemd = load_launcher()
        launcher._original_control.return_value = {"phoenix-simurgh-setup": "ok"}
        result = systemd.control("enable", ("simurgh-setup",))
        self.assertEqual(result["simurgh-setup"], "ok")
        launcher._original_control.assert_called_once_with("enable", ("phoenix-simurgh-setup",))

        launcher._original_control.reset_mock()
        systemd._run.return_value = (0, "loaded")
        refused = systemd.control("enable", ("simurgh-setup",))
        self.assertIn("already owned", refused["simurgh-setup"])
        launcher._original_control.assert_not_called()

        systemd._run.return_value = (1, "Unit simurgh-setup.service could not be found.")
        launcher._original_control.return_value = {"phoenix-simurgh-setup": "ok"}
        missing = systemd.control("enable", ("simurgh-setup",))
        self.assertEqual(missing["simurgh-setup"], "ok")
        launcher._original_control.assert_called_once_with("enable", ("phoenix-simurgh-setup",))

    def test_selected_role_is_recorded_after_upstream_prepares_its_unit(self):
        launcher, _cli, _menu, systemd = load_launcher()
        with tempfile.TemporaryDirectory() as directory:
            systemd.UNIT_DIR = Path(directory) / "units"
            systemd.UNIT_DIR.mkdir()
            home_path = Path(directory) / "home"
            home_path.mkdir()
            logs = home_path / "logs"
            logs.mkdir()
            exit_cfg = home_path / "exit.toml"
            exit_cfg.touch()
            home = SimpleNamespace(path=home_path, logs=logs, exit_cfg=exit_cfg,
                                  relay_cfg=home_path / "relay.toml")
            launcher._original_install_services.return_value = {
                "systemd": True, "units": [], "actions": {"simurgh-exit": "ok"},
                "write": True,
            }
            report = systemd.install_services(home, roles=("exit",), start=False)
            self.assertTrue(report["systemd"])
            self.assertEqual((home_path / "phoenix-role").read_text().strip(), "exit")
            self.assertEqual((home_path / "phoenix-role").stat().st_mode & 0o777, 0o600)


class BundledMenuTests(unittest.TestCase):
    def test_menu_routes_only_to_the_phoenix_relay_unit(self):
        launcher, cli, menu, systemd = load_launcher()
        self.assertEqual(cli._service_names("relay"), ("phoenix-simurgh-relay",))

        with patch.object(launcher, "running_in_docker", return_value=False):
            self.assertIn("active", menu.service_state("simurgh-relay"))
            systemd.service_state.assert_called_once_with("phoenix-simurgh-relay")
            menu.service_action("relay", "restart")
            launcher._original_control.assert_called_once_with("restart", ("phoenix-simurgh-relay",))

            output = io.StringIO()
            with redirect_stdout(output):
                menu.service_action("exit", "stop")
            self.assertIn("only manages the detected Simurgh role", output.getvalue())
            launcher._original_control.assert_called_once()

    def test_foreign_role_controls_only_the_phoenix_exit_unit(self):
        launcher, cli, menu, systemd = load_launcher("exit")
        self.assertEqual(cli._service_names("exit"), ("phoenix-simurgh-exit",))
        self.assertEqual(cli._service_names("relay"), ())
        with patch.object(launcher, "running_in_docker", return_value=False):
            self.assertIn("active", menu.service_state("simurgh-exit"))
            systemd.service_state.assert_called_once_with("phoenix-simurgh-exit")
            menu.service_action("exit", "restart")
            launcher._original_control.assert_called_once_with("restart", ("phoenix-simurgh-exit",))

    def test_menu_refuses_to_restart_container_managed_service(self):
        launcher, _cli, menu, systemd = load_launcher()
        output = io.StringIO()
        with patch.object(launcher, "running_in_docker", return_value=True), redirect_stdout(output):
            menu.service_action("relay", "restart")
        self.assertIn("managed by Docker", output.getvalue())
        launcher._original_control.assert_not_called()
        self.assertEqual(menu.pause.call_count, 1)


if __name__ == "__main__":
    unittest.main()
