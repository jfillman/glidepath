#!/usr/bin/env python3
"""run-testworkflow forwards app-secrets AND dex-check-ride-credentials into testkube/<app>-app-secrets.

Runs the real step text (the block between `materializing` and `BEGIN testworkflow-policy`) with a fake
kubectl on PATH, so what is asserted is what the Task does: both Secrets merged, the Dex keys winning,
either one optional, nothing patched when neither exists.
"""
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

TASK = pathlib.Path(__file__).resolve().parents[1] / "templates" / "tasks" / "run-testworkflow.yaml"

FAKE_KUBECTL = r"""#!/bin/bash
# kubectl get secret <name> -o json  -> $FIXTURES/<name>.json, or fail like a missing Secret
# kubectl -n testkube patch secret <n> --type merge -p <json> -> record the call
if [[ "$1" == "get" && "$2" == "secret" ]]; then
  f="$FIXTURES/$3.json"
  [[ -f "$f" ]] || { echo "Error from server (NotFound)" >&2; exit 1; }
  cat "$f"; exit 0
fi
if [[ "$1" == "-n" && "$3" == "patch" ]]; then
  printf '%s' "$9" > "$FIXTURES/patched.json"; echo "$5" > "$FIXTURES/patched-name"; exit 0
fi
echo "unexpected kubectl $*" >&2; exit 2
"""


def step_block():
    text = TASK.read_text()
    start = text.index('        echo "materializing')
    end = text.index("        # BEGIN testworkflow-policy")
    return text[start:end]


def run(secrets):
    with tempfile.TemporaryDirectory() as d:
        d = pathlib.Path(d)
        (d / "bin").mkdir()
        k = d / "bin" / "kubectl"
        k.write_text(FAKE_KUBECTL)
        k.chmod(0o755)
        for name, data in secrets.items():
            (d / f"{name}.json").write_text(json.dumps({"kind": "Secret", "metadata": {"name": name}, "data": data}))
        script = 'set -euo pipefail\nSECRET_NAME="demo-app-secrets"\n' + step_block()
        env = {**os.environ, "PATH": f"{d / 'bin'}:{os.environ['PATH']}", "FIXTURES": str(d)}
        out = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True, check=True)
        patched = d / "patched.json"
        return (json.loads(patched.read_text()) if patched.exists() else None), out.stdout


class TestSecretForward(unittest.TestCase):
    def test_both_secrets_are_merged_and_the_dex_keys_win(self):
        patch, _ = run({
            "app-secrets": {"slack-webhook-url": "c2xhY2s=", "DEX_CLIENT_ID": "aGFuZC1tYWRl"},
            "dex-check-ride-credentials": {"DEX_TOKEN_URL": "dXJs", "DEX_CLIENT_ID": "aWQ=", "DEX_CLIENT_SECRET": "c2VjcmV0"},
        })
        self.assertEqual(patch, {"data": {
            "slack-webhook-url": "c2xhY2s=", "DEX_TOKEN_URL": "dXJs", "DEX_CLIENT_ID": "aWQ=", "DEX_CLIENT_SECRET": "c2VjcmV0",
        }})

    def test_dex_credentials_alone(self):
        patch, _ = run({"dex-check-ride-credentials": {"DEX_TOKEN_URL": "dXJs", "DEX_CLIENT_ID": "aWQ=", "DEX_CLIENT_SECRET": "c2VjcmV0"}})
        self.assertEqual(set(patch["data"]), {"DEX_TOKEN_URL", "DEX_CLIENT_ID", "DEX_CLIENT_SECRET"})

    def test_app_secrets_alone_is_unchanged_behaviour(self):
        patch, _ = run({"app-secrets": {"slack-webhook-url": "c2xhY2s="}})
        self.assertEqual(patch, {"data": {"slack-webhook-url": "c2xhY2s="}})

    def test_neither_secret_patches_nothing(self):
        patch, out = run({})
        self.assertIsNone(patch)
        self.assertIn("leaving demo-app-secrets empty", out)

    def test_an_empty_data_block_does_not_break_the_merge(self):
        patch, _ = run({"dex-check-ride-credentials": None, "app-secrets": {"a": "Yg=="}})
        self.assertEqual(patch, {"data": {"a": "Yg=="}})


if __name__ == "__main__":
    unittest.main()
