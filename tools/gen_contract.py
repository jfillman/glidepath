#!/usr/bin/env python3
"""Generates contract/glidepath-contract.json: everything a client needs to use Glidepath without
reading its source (docs/admin/contract.md).

    python3 tools/gen_contract.py           # write it
    python3 tools/gen_contract.py --check   # exit 1 if the committed file is stale (CI)

Sources, all in this repo:
  schemas/cicd.schema.json                    every cicd.yaml field: type, enum, default, description
  schemas/cicd.meta.yaml                      owner, risk and agent scope per field; states; known failures
  charts/glidepath-catalog/values.yaml        releaseGuardrails: the release gates
  api/openapi.yaml                            the API's operations and their tiers (x-glidepath-tier)

Fails when a schema field matches no `fields` pattern in the sidecar, so a new field can't ship
without someone deciding whether an agent may touch it.
Needs: python3, pyyaml.
"""
import argparse
import json
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tools"))
from cicd_scope import rule_for  # noqa: E402

SCHEMA = ROOT / "schemas" / "cicd.schema.json"
META = ROOT / "schemas" / "cicd.meta.yaml"
CATALOG_VALUES = ROOT / "charts" / "glidepath-catalog" / "values.yaml"
OPENAPI = ROOT / "api" / "openapi.yaml"
OUT = ROOT / "contract" / "glidepath-contract.json"

CONTRACT_VERSION = 1
METHODS = ("get", "post", "put", "patch", "delete")


def resolve(schema, node):
    """Follow a local $ref (#/properties/...)."""
    seen = 0
    while isinstance(node, dict) and "$ref" in node:
        target = schema
        for seg in node["$ref"].lstrip("#/").split("/"):
            target = target[seg]
        node = target
        seen += 1
        if seen > 20:
            raise ValueError("ref loop")
    return node


def walk(schema, node, pointer, out):
    """Every field the schema defines, as (pointer, node). Array items and map values are `*`; the
    branches of a oneOf are merged, so both forms of a pipeline appear."""
    node = resolve(schema, node)
    branches = [resolve(schema, b) for b in node.get("oneOf", []) + node.get("anyOf", [])]
    if branches:
        for b in branches:
            walk(schema, {**{k: v for k, v in node.items() if k not in ("oneOf", "anyOf")}, **b}, pointer, out)
        return
    props = node.get("properties") or {}
    items = node.get("items") if isinstance(node.get("items"), dict) else None
    extra = node.get("additionalProperties") if isinstance(node.get("additionalProperties"), dict) else None
    if pointer:
        out.setdefault(pointer, node)
    for k, v in props.items():
        walk(schema, v, f"{pointer}/{k}", out)
    if items is not None:
        walk(schema, items, f"{pointer}/*", out)
    if extra is not None:
        walk(schema, extra, f"{pointer}/*", out)


def field_entry(pointer, node, rule):
    e = {"pointer": pointer}
    t = node.get("type")
    if t:
        e["type"] = t
    if "enum" in node:
        e["enum"] = node["enum"]
    if "pattern" in node:
        e["pattern"] = node["pattern"]
    if "default" in node:
        e["default"] = node["default"]
    if node.get("description"):
        e["description"] = " ".join(node["description"].split())
    for k in ("owner", "risk", "agentScope", "order", "why"):
        if k in rule:
            e[k] = rule[k]
    return e


def scope_rules(meta, fields_by_pointer):
    rules = []
    for r in meta["fields"]:
        out = {"pattern": r["pattern"], "agentScope": r["agentScope"]}
        if r["agentScope"] == "tighten":
            out["order"] = r["order"]
            if "default" in r:
                out["default"] = r["default"]
            elif "default" in fields_by_pointer.get(r["pattern"], {}):
                out["default"] = fields_by_pointer[r["pattern"]]["default"]
        if r.get("why"):
            out["why"] = r["why"]
        rules.append(out)
    return rules


def gates():
    values = yaml.safe_load(CATALOG_VALUES.read_text())
    out = []
    for g in values.get("releaseGuardrails") or []:
        e = {"name": g["name"], "description": " ".join(str(g.get("description", "")).split()),
             "status": g.get("status"), "class": g.get("class"), "required": g.get("required", True) is not False}
        if g.get("targets"):
            e["targets"] = g["targets"]
        out.append(e)
    return out


def api_block():
    if not OPENAPI.exists():
        return None
    spec = yaml.safe_load(OPENAPI.read_text())
    actions = []
    for path, ops in (spec.get("paths") or {}).items():
        for method in METHODS:
            op = ops.get(method)
            if not op:
                continue
            tier = op.get("x-glidepath-tier")
            if tier is None:
                raise SystemExit(f"api/openapi.yaml: {method.upper()} {path} has no x-glidepath-tier")
            actions.append({"operationId": op.get("operationId"), "method": method.upper(), "path": path,
                            "tier": tier, "summary": op.get("summary", "")})
    return {"spec": "api/openapi.yaml", "version": spec.get("info", {}).get("version"), "actions": actions}


def build():
    schema = json.loads(SCHEMA.read_text())
    meta = yaml.safe_load(META.read_text())
    walked = {}
    walk(schema, schema, "", walked)
    # Leaves only: an object with fields of its own is a container, described by its fields.
    leaves = {p: n for p, n in walked.items() if not any(q.startswith(p + "/") for q in walked)}
    unmatched, fields = [], []
    for pointer, node in leaves.items():
        rule = rule_for(meta["fields"], pointer)
        if rule is None:
            unmatched.append(pointer)
            continue
        fields.append(field_entry(pointer, node, rule))
    if unmatched:
        raise SystemExit("schemas/cicd.meta.yaml: no `fields` pattern covers " + ", ".join(unmatched))
    by_pointer = {f["pointer"]: f for f in fields}
    contract = {
        "$comment": "GENERATED by tools/gen_contract.py from schemas/, charts/glidepath-catalog/values.yaml and api/. Do not edit.",
        "contractVersion": CONTRACT_VERSION,
        "cicd": {
            "apiVersion": schema["properties"]["apiVersion"]["enum"][0],
            "kind": schema["properties"]["kind"]["enum"][0],
            "schema": "schemas/cicd.schema.json",
            "fields": fields,
        },
        "scope": {
            "rules": scope_rules(meta, by_pointer),
            "structural": {
                "GP-SCOPE-002": "a flow loses a check-ride step or a required test step",
                "GP-SCOPE-003": "a Flight environment is added, removed or changed, or an environment changes tier",
            },
            "implementation": "tools/cicd_scope.py",
        },
        "gates": gates(),
        "states": meta["states"],
        "knownFailures": meta["knownFailures"],
    }
    api = api_block()
    if api:
        contract["api"] = api
    return json.dumps(contract, indent=2, ensure_ascii=False) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--check", action="store_true", help="fail if the committed contract is stale")
    args = ap.parse_args()
    text = build()
    if args.check:
        if not OUT.exists() or OUT.read_text() != text:
            print(f"{OUT.relative_to(ROOT)} is stale: run python3 tools/gen_contract.py and commit it", file=sys.stderr)
            return 1
        print(f"{OUT.relative_to(ROOT)} is current")
        return 0
    OUT.parent.mkdir(exist_ok=True)
    OUT.write_text(text)
    print(f"wrote {OUT.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
