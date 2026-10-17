#!/usr/bin/env python3
"""Every docs/user/examples/*.yaml is a working cicd.yaml: schema-valid and rendered by glidepath-app without error.

The examples README always promised this ("if it's in this folder, it works") but nothing checked it, and on
2026-10-17 05-release-on-tag.yaml failed to render: it released to `staging` without declaring it in
deploy.environments, which the chart refuses. Each file is validated against schemas/cicd.schema.json and rendered
with `helm template` plus tests/fixtures/control-plane-values.yaml (what the control plane passes a real app), the
way Argo CD renders <app>-cicd. Needs helm, pyyaml and jsonschema.
"""
import glob
import json
import os
import subprocess
import sys

import jsonschema
import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CHART = os.path.join(ROOT, "charts", "glidepath-app")
FIXTURE = os.path.join(CHART, "tests", "fixtures", "control-plane-values.yaml")


def main():
    schema = json.load(open(os.path.join(ROOT, "schemas", "cicd.schema.json")))
    validator = jsonschema.Draft202012Validator(schema)
    files = sorted(glob.glob(os.path.join(ROOT, "docs", "user", "examples", "*.yaml")))
    if not files:
        sys.exit("no examples found - the test would be vacuous")
    failed = 0
    for path in files:
        name = os.path.basename(path)
        problems = [f"schema: {e.message}" for e in validator.iter_errors(yaml.safe_load(open(path)))]
        r = subprocess.run(
            ["helm", "template", "example-app-cicd", CHART, "--namespace", "app-example-app-cicd", "-f", path, "-f", FIXTURE],
            capture_output=True, text=True,
        )
        if r.returncode != 0:
            problems.append("render: " + r.stderr.strip().splitlines()[0][:300])
        else:
            docs = [d for d in yaml.safe_load_all(r.stdout) if d]
            if not docs:
                problems.append("render: produced no objects")
        if problems:
            failed += 1
            for p in problems[:3]:
                print(f"FAIL {name}: {p}")
        else:
            print(f"ok   {name}")
    print(f"{len(files)} examples, {failed} failing")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
