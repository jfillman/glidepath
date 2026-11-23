#!/usr/bin/env python3
"""Every onboarding template whose Pipeline has a `finally` block sets timeouts.tasks, .finally and .pipeline with
tasks >= finally and pipeline >= tasks + finally (Tekton counts `finally` against `tasks`; I-85,
docs/admin/release-guardrails.md "Timeouts: always split tasks/finally"). A template for a Pipeline with no
`finally` must still set timeouts.pipeline."""
import glob, os, re, sys

import yaml

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..")
TPL = os.path.join(ROOT, "charts/glidepath-app/files/onboarding-templates")
PIPES = os.path.join(ROOT, "charts/glidepath-catalog/templates/pipelines")


def minutes(v):
    m = re.fullmatch(r"([0-9]+)(m|h)", v)
    if not m:
        raise ValueError(f"unparseable duration {v!r}")
    return int(m.group(1)) * (60 if m.group(2) == "h" else 1)


def has_finally(pipeline):
    p = os.path.join(PIPES, pipeline + ".yaml")
    if not os.path.exists(p):
        return None
    return re.search(r"^  finally:", open(p).read(), re.M) is not None


errors, n = [], 0
for f in sorted(glob.glob(TPL + "/*/*.yaml")):
    n += 1
    text = open(f).read()
    rel = os.path.relpath(f, TPL)
    try:
        t = yaml.safe_load(text)["spec"]["timeouts"]
    except (KeyError, TypeError) as e:
        errors.append(f"{rel}: no spec.timeouts ({e!r})")
        continue
    bad = [k for k, v in t.items() if not isinstance(v, str) or not re.fullmatch(r"[0-9]+[mh]", v)]
    if bad or set(t) - {"tasks", "finally", "pipeline"}:
        errors.append(f"{rel}: timeouts must be whole minute/hour values (Nm or Nh) for tasks/finally/pipeline: {t}")
        continue
    if "pipeline" not in t:
        errors.append(f"{rel}: timeouts.pipeline missing")
        continue
    nm = re.search(r"\{ name: name, value: ([\w-]+) \}", text)
    fin = has_finally(nm.group(1)) if nm else None
    if fin is None:
        fin = "finally" in t  # pipeline not in this chart's catalog: judge by what the template declares
    if not fin:
        continue
    if set(t) != {"tasks", "finally", "pipeline"}:
        errors.append(f"{rel}: Pipeline has finally but timeouts sets only {sorted(t)}")
        continue
    ta, fi, pi = minutes(t["tasks"]), minutes(t["finally"]), minutes(t["pipeline"])
    if ta < fi:
        errors.append(f"{rel}: tasks {t['tasks']} < finally {t['finally']}")
    if pi < ta + fi:
        errors.append(f"{rel}: pipeline {t['pipeline']} < tasks + finally ({ta + fi}m)")
    if ta < fi + 2:
        errors.append(f"{rel}: tasks {t['tasks']} leaves no check time beyond finally {t['finally']}")

assert n > 10, f"only {n} templates found: vacuous"
if errors:
    print("\n".join(errors))
    sys.exit(1)
print(f"ok: {n} onboarding templates checked")
