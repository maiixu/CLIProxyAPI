"""Offline regression coverage for Codex runtime and app-server launch arguments."""
import json
import os
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest.mock import patch


WRAPPER = Path(__file__).with_name("codex-zed")
PROFILE = Path(__file__).with_name("zed-sol.config.toml")
KEY_PATH = Path("/Users/maixu/.config/cliproxyapi/client-key")


class CodexZedWrapperTest(unittest.TestCase):
    def launch(self, arguments):
        read_text = Path.read_text
        is_file = Path.is_file
        with tempfile.TemporaryDirectory(prefix="codex-zed-wrapper-test-") as directory:
            profile_home = Path(directory)
            (profile_home / "zed-sol.config.toml").write_text(PROFILE.read_text())
            environment = {
                "CODEX_HOME": directory,
                "OPENAI_API_KEY": "must-not-leak",
                "CODEX_API_KEY": "must-not-leak",
                "OPENAI_BASE_URL": "https://must-not-use.invalid/v1",
                "MCP_LAYER_SENTINEL": "preserved",
            }
            def read(path, *args, **kwargs):
                return "local-test-key\n" if path == KEY_PATH else read_text(path, *args, **kwargs)
            def exists(path):
                return True if path == KEY_PATH else is_file(path)
            with patch.dict(os.environ, environment, clear=True), \
                 patch("sys.argv", [str(WRAPPER), *arguments]), \
                 patch.object(Path, "read_text", read), \
                 patch.object(Path, "is_file", exists), \
                 patch("os.execve", side_effect=RuntimeError("captured exec")) as execute:
                with self.assertRaisesRegex(RuntimeError, "captured exec"):
                    runpy.run_path(str(WRAPPER), run_name="__main__")
            executable, argv, env = execute.call_args.args
            self.assertEqual(executable, "/opt/homebrew/bin/codex")
            self.assertEqual(env["CODEX_HOME"], directory)
            self.assertEqual(env["MCP_LAYER_SENTINEL"], "preserved")
            self.assertEqual(env["ZED_GATEWAY_API_KEY"], "local-test-key")
            for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"):
                self.assertNotIn(key, env)
            return argv

    def test_app_server_loads_profile_as_supported_nested_overrides(self):
        argv = self.launch(["app-server", "--enable", "goals"])
        self.assertNotIn("--profile", argv)
        self.assertNotIn("--ignore-user-config", argv)
        values = {}
        for index, argument in enumerate(argv[:-1]):
            if argument == "-c":
                key, value = argv[index + 1].split("=", 1)
                values[key] = json.loads(value)
        self.assertEqual(values["model"], "zed/gpt-5.6-sol")
        self.assertEqual(values["model_provider"], "zed-gateway")
        self.assertEqual(values["model_catalog_json"], "/Users/maixu/.codex/zed-sol.models.json")
        self.assertFalse(values["model_providers.zed-gateway.requires_openai_auth"])
        self.assertEqual(values["model_providers.zed-gateway.env_key"], "ZED_GATEWAY_API_KEY")
        self.assertFalse(values["features.apps"])
        self.assertFalse(values["features.multi_agent"])
        self.assertFalse(values["features.enable_request_compression"])
        self.assertEqual(argv[1], "app-server")
        self.assertEqual(argv[-2:], ["--enable", "goals"])

    def test_version_discovery_does_not_load_profile_or_credentials(self):
        with patch("sys.argv", [str(WRAPPER), "--version"]), \
             patch.object(Path, "read_text", side_effect=AssertionError("Unexpected file read")), \
             patch("os.execv", side_effect=RuntimeError("captured exec")) as execute:
            with self.assertRaisesRegex(RuntimeError, "captured exec"):
                runpy.run_path(str(WRAPPER), run_name="__main__")
        self.assertEqual(execute.call_args.args, (
            "/opt/homebrew/bin/codex", ["/opt/homebrew/bin/codex", "--version"],
        ))

    def test_exec_keeps_profile_selection(self):
        argv = self.launch(["exec", "--ephemeral", "hello"])
        self.assertEqual(argv[1:], ["--profile", "zed-sol", "exec", "--ephemeral", "hello"])

    def test_app_server_preserves_explicit_caller_overrides(self):
        argv = self.launch(["app-server", "-c", 'model_reasoning_effort="high"'])
        self.assertEqual(argv[1], "app-server")
        self.assertEqual(argv[-2:], ["-c", 'model_reasoning_effort="high"'])
        self.assertIn('model_reasoning_effort="medium"', argv[2:-2])


if __name__ == "__main__":
    unittest.main()
