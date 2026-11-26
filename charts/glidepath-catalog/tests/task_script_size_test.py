#!/usr/bin/env python3
"""No catalog Task's step scripts may come near the size Tekton can hand to a pod.

Tekton's place-scripts init container gets every script of a Task's pod (each step's, StepAction steps included, and each sidecar's)
in ONE argument (`sh -c <text>`): per script a few lines of shell around the script, base64-encoded. Linux caps a single argument at
MAX_ARG_STRLEN = 32 pages = 131072 bytes (4 KiB pages; the lab's arm64 node has 4096-byte pages too). Past that every pod of the Task
fails with "exec /bin/sh: argument list too long" before any step runs.

It happened twice to clearance-gate:
  - glidepath#247 inlined files/lib/release-source.sh into its fetch step: 138044 bytes (gitops-parachute#14).
  - CG-3 (3277dc5) added the classify step, a StepAction resolved into the Task at run time. This test then read only the Task's own
    `script:` fields, never saw the StepAction's ~11 KB, and passed while the live argument was 131559 bytes on eks-dev (2026-11-26).
    The Task was split in two (clearance-gate-fetch, clearance-gate), and this test now inlines every StepAction a step refers to.

The estimate per script is what Tekton (pkg/pod/script.go) does: a script without a shebang gets the default preamble
("#!/bin/sh\nset -e\n"), the result is base64-encoded, and PER_SCRIPT_OVERHEAD covers the heredoc and decode-script lines around it
(measured: 6 steps, 138044 live vs 136944 of base64; the 2026-11-26 failure, 7 steps, estimates within 1% of its 131559). Tekton also
substitutes $(params.*), $(results.*.path) and the like into a script first; that changes a few bytes, not kilobytes.

Every Task, rendered with the largest realistic values, must stay under LIMIT = 75% of the cap: the headroom is for values a
cluster sets that the renders here do not (longer lists, hosts) and for the next feature. Library text a step needs belongs in the
toolbox image ($PLATFORM_LIB) or in a step env var (as RELEASE_SOURCE_LIB and PROTECTION_LIB do), not in the script; a Task that still
does not fit is split into two Tasks that hand each other data on the run's workspace (clearance-gate-fetch / clearance-gate).

Run:  python3 charts/glidepath-catalog/tests/task_script_size_test.py   (needs helm, pyyaml; -v prints the largest Tasks)
"""
import base64
import os
import subprocess
import sys
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
VALUES = os.path.join(CATALOG, "tests", "values.example.yaml")
HARD_CAP = 131072  # MAX_ARG_STRLEN: 32 * 4096
LIMIT = HARD_CAP * 75 // 100  # 98304
PER_SCRIPT_OVERHEAD = 200  # place-scripts' own text around each script
DEFAULT_PREAMBLE = "#!/bin/sh\nset -e\n"  # Tekton's, for a script with no shebang

# The renders that add the most script text: the example values, and the same with every optional feature on (what a lab turns on).
RENDERS = {
    "example values": [],
    "promotion on": [
        "--set", "promotion.enabled=true", "--set", "promotion.pinDigest=true",
        "--set", "registries.flight.namespace=riley-roooo", "--set", "promotion.promoter.repo=riley-roooo/flight-promoter",
        "--set-json", 'releaseGuardrailOverridesByApp={"backstage":{"image-scan":{"required":false}}}',
    ],
    "itsm on": ["--set", "itsm.provider=webhook"],
}


def render(extra):
    out = subprocess.run(["helm", "template", "t", CATALOG, "-f", VALUES, "--namespace", "platform-catalog", *extra],
                         capture_output=True, text=True, check=True).stdout
    return [d for d in yaml.safe_load_all(out) if d]


def ref_name(ref):
    """The StepAction a step's `ref` names: a local ref (name) or a resolver ref (the cluster resolver's `name` param)."""
    if ref.get("name"):
        return ref["name"]
    names = [p["value"] for p in ref.get("params", []) if p["name"] == "name"]
    kinds = [p["value"] for p in ref.get("params", []) if p["name"] == "kind"]
    assert names and kinds == ["stepaction"], f"a step ref this test cannot resolve: {ref}"
    return names[0]


def script_bytes(script):
    """One script's share of the place-scripts argument, as Tekton builds it."""
    if not script:
        return 0
    if not script.startswith("#!"):
        script = DEFAULT_PREAMBLE + script
    return len(base64.b64encode(script.encode())) + PER_SCRIPT_OVERHEAD


def scripts_of(task, stepactions):
    """[(step name, script)] of the Task's pod: its steps (a StepAction step's script inlined, as Tekton resolves it) and sidecars."""
    out = []
    spec = task["spec"]
    for s in spec.get("steps", []):
        if "ref" in s:
            name = ref_name(s["ref"])
            assert name in stepactions, f"{task['metadata']['name']}/{s['name']}: StepAction {name} is not in the catalog render"
            out.append((s["name"] + " (StepAction " + name + ")", stepactions[name]["spec"].get("script", "")))
        else:
            out.append((s["name"], s.get("script", "")))
    for s in spec.get("sidecars", []):
        out.append(("sidecar " + s["name"], s.get("script", "")))
    return out


def estimates(docs):
    """{Task name: (total estimated bytes, [(step, bytes)])}."""
    stepactions = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "StepAction"}
    sizes = {}
    for d in docs:
        if d.get("kind") == "Task":
            parts = [(n, script_bytes(s)) for n, s in scripts_of(d, stepactions)]
            sizes[d["metadata"]["name"]] = (sum(b for _, b in parts), parts)
    return sizes


def report(sizes, n=8):
    lines = []
    for name, (total, parts) in sorted(sizes.items(), key=lambda kv: -kv[1][0])[:n]:
        lines.append(f"{name:28s} {total:7d}  {100 * total / HARD_CAP:5.1f}% of {HARD_CAP}  " + ", ".join(f"{s} {b}" for s, b in parts))
    return "\n".join(lines)


class TaskScriptSize(unittest.TestCase):
    def test_every_task_fits(self):
        self.assertLess(LIMIT, HARD_CAP)
        for label, extra in RENDERS.items():
            docs = render(extra)
            sizes = estimates(docs)
            self.assertGreater(len(sizes), 10, "rendered too few Tasks: the guard is looking at nothing")
            self.assertIn("clearance-gate", sizes)
            self.assertIn("clearance-gate-fetch", sizes)
            if "-v" in sys.argv or os.environ.get("VERBOSE"):
                print(f"\n[{label}] estimated place-scripts argument, largest Tasks (limit {LIMIT}):\n" + report(sizes))
            over = {n: s for n, (s, _) in sizes.items() if s > LIMIT}
            self.assertFalse(over, f"{label}: Task scripts over {LIMIT} estimated place-scripts bytes (75% of the {HARD_CAP} Linux caps one "
                                   f"argument at):\n{report({n: sizes[n] for n in over}, n=len(over))}")

    def test_stepaction_scripts_are_counted(self):
        # The CG-3 regression: a step with a `ref` has no script of its own, and a guard that reads only `script:` misses it.
        docs = render([])
        sizes = estimates(docs)
        classify = [b for s, b in sizes["clearance-gate-fetch"][1] if s.startswith("classify (StepAction classify-change)")]
        self.assertEqual(len(classify), 1)
        self.assertGreater(classify[0], 10000, "the classify StepAction's script was not inlined")

    def test_the_guard_would_have_caught_the_one_task_gate(self):
        # clearance-gate's fetch and decide halves, as the one Task they were until 2026-11-26 (131559 bytes live, "argument list too long"):
        # the estimate must put that shape over the limit, or this guard could not have caught it.
        sizes = estimates(render([]))
        joined = sizes["clearance-gate-fetch"][0] + sizes["clearance-gate"][0]
        self.assertGreater(joined, LIMIT)
        self.assertGreater(joined, HARD_CAP * 95 // 100, f"the one-Task estimate ({joined}) is far from the 131559 measured live")


if __name__ == "__main__":
    unittest.main(argv=[a for a in sys.argv if a != "-v"], verbosity=2 if "-v" in sys.argv else 1)
