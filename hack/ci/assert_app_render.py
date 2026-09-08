#!/usr/bin/env python3
"""Fixture-matrix assertions for charts/glidepath-app's `helm template` output.

Used by .github/workflows/app-chart-ci.yaml to verify the conditional-rendering
logic (glidepath-app.hasStage in templates/_helpers.tpl) actually gates
resources on cicd.yaml's declared pipeline stages - the direct regression test for the
live-confirmed bug this whole chart exists to fix (a release PipelineRun firing for an
Application that never declared a release stage).

Usage: assert_app_render.py <rendered.yaml> [--present KIND[:NAME] ...]
       [--absent KIND[:NAME] ...] [--count KIND:NAME N] [--pvc-size KIND:NAME SIZE]

KIND[:NAME] with no NAME matches any resource of that kind (at least one, for
--present; none, for --absent).
"""
import argparse
import sys
import yaml


def load_docs(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]


def matches(doc, kind, name):
    if doc.get("kind") != kind:
        return False
    if name is None:
        return True
    return doc.get("metadata", {}).get("name") == name


def parse_ref(ref):
    if ":" in ref:
        kind, name = ref.split(":", 1)
        return kind, name
    return ref, None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("rendered")
    parser.add_argument("--present", action="append", nargs="+", default=[])
    parser.add_argument("--absent", action="append", nargs="+", default=[])
    parser.add_argument("--count", action="append", nargs=2, default=[])
    parser.add_argument("--pvc-size", action="append", nargs=2, default=[])
    args = parser.parse_args()

    docs = load_docs(args.rendered)
    failures = []

    present_refs = [r for group in args.present for r in group]
    absent_refs = [r for group in args.absent for r in group]

    for ref in present_refs:
        kind, name = parse_ref(ref)
        if not any(matches(d, kind, name) for d in docs):
            failures.append(f"expected PRESENT but missing: {ref}")

    for ref in absent_refs:
        kind, name = parse_ref(ref)
        found = [d for d in docs if matches(d, kind, name)]
        if found:
            names = [d.get("metadata", {}).get("name") for d in found]
            failures.append(f"expected ABSENT but found {len(found)}: {ref} (names: {names})")

    for ref, expected_n in args.count:
        kind, name = parse_ref(ref)
        found = [d for d in docs if matches(d, kind, name)]
        if len(found) != int(expected_n):
            failures.append(f"expected count {expected_n} for {ref}, got {len(found)}")

    for ref, expected_size in args.pvc_size:
        kind, name = parse_ref(ref)
        found = [d for d in docs if matches(d, kind, name)]
        if not found:
            failures.append(f"--pvc-size: no resource found for {ref}")
        else:
            actual = found[0].get("spec", {}).get("resources", {}).get("requests", {}).get("storage")
            if actual != expected_size:
                failures.append(f"--pvc-size: {ref} expected {expected_size}, got {actual}")

    if failures:
        print(f"FAILED ({len(failures)}):")
        for f in failures:
            print(f"  - {f}")
        sys.exit(1)

    print(f"OK: {len(present_refs)} present + {len(absent_refs)} absent + "
          f"{len(args.count)} count + {len(args.pvc_size)} pvc-size assertions passed "
          f"against {len(docs)} rendered resources")


if __name__ == "__main__":
    main()
