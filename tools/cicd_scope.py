"""What an agent may change in a cicd.yaml: the GP-SCOPE rules (docs/admin/contract.md).

Standard library only, and no import from this repo: Autopilot's Clearance vendors this file byte
for byte (autopilot/src/clearance/vendor/cicd_scope.py, checked by a drift test there), so the
gate an agent meets before it opens a pull request and the check that pull request then meets in
CI are the same code. Change it here, then copy it there.

    problems = scope_problems(base, head, contract["scope"])
    needs = needs_person(base, head, contract["scope"])   # what only a person's merge may land

`base` and `head` are the parsed cicd.yaml before and after the change (a dict, or None for a file
that doesn't exist yet). `contract["scope"]` is the `scope` block of contract/glidepath-contract.json.
Each problem is a dict: pointer, rule, message, hint. Each needs-person entry is a dict: pointer, change, message.

Scope classes (a rule's `agentScope`): open, tighten, human, and propose: a change an agent may make only in a
pull request that a person merges. A propose change is not a problem, so `scope_problems` stays empty for it;
`needs_person` lists it, and the caller (the tool that opens the PR) marks the PR human-merge. A rule may set
`proposeOn` (a list of "added", "changed", "removed"): the kinds of change that are propose; any other kind of
change to that field is human. So a NEW Flight environment's `cluster` is propose and an EXISTING one's is not.

Rules:
  GP-SCOPE-001  a human-only field changed
  GP-SCOPE-002  a gate loosened: a tighten-only field moved the loose way, or a flow lost a
                check-ride step or a required test step
  GP-SCOPE-003  a Flight environment removed, or an environment's tier changed (a Flight environment
                added or changed, and the release step into one, is propose: see needs_person)
"""
from __future__ import annotations

from typing import Any

MISSING = object()


# --- JSON pointers ---------------------------------------------------------------------------

def _esc(key: Any) -> str:
    return str(key).replace("~", "~0").replace("/", "~1")


def _segments(pointer: str) -> list[str]:
    if pointer in ("", "/"):
        return []
    return [s.replace("~1", "/").replace("~0", "~") for s in pointer.lstrip("/").split("/")]


def pointer_matches(pattern: str, pointer: str) -> bool:
    """`*` matches one segment, `**` any number; a pattern also matches everything under it."""
    pat, ptr = _segments(pattern), _segments(pointer)

    def go(i: int, j: int) -> bool:
        if i == len(pat):
            return True  # the pattern is a prefix of the pointer: everything under it matches
        if pat[i] == "**":
            return any(go(i + 1, k) for k in range(j, len(ptr) + 1))
        if j == len(ptr):
            return False
        return (pat[i] == "*" or pat[i] == ptr[j]) and go(i + 1, j + 1)

    return go(0, 0)


def kind_of(base: Any, head: Any) -> str:
    return "removed" if head is MISSING else "added" if base is MISSING else "changed"


def effective_scope(rule: dict | None, kind: str) -> str:
    """The scope a change of this kind has under the rule: a propose rule that names `proposeOn` is propose only for
    those kinds and human for the others; no rule is human."""
    if rule is None:
        return "human"
    sc = rule["agentScope"]
    if sc == "propose" and kind not in (rule.get("proposeOn") or ("added", "changed", "removed")):
        return "human"
    return sc


def rule_for(rules: list[dict], pointer: str) -> dict | None:
    """The first rule whose pattern matches the pointer."""
    for r in rules:
        if pointer_matches(r["pattern"], pointer):
            return r
    return None


# --- the diff --------------------------------------------------------------------------------

def _named(items: list) -> bool:
    names = [i.get("name") for i in items if isinstance(i, dict)]
    return len(names) == len(items) and all(isinstance(n, str) for n in names) and len(set(names)) == len(names)


def _pair_list(base: list, head: list) -> list[tuple[str, Any, Any]]:
    """Pair the items of two lists: by `name` when every item has a unique one (environments), else
    identical items first, then the rest in order, so inserting a step doesn't look like every later
    step changed."""
    if _named(base) and _named(head):
        b = {i["name"]: i for i in base}
        h = {i["name"]: i for i in head}
        keys = list(dict.fromkeys([*b, *h]))
        return [(k, b.get(k, MISSING), h.get(k, MISSING)) for k in keys]
    used: set[int] = set()
    pairs: list[tuple[str, Any, Any]] = []
    rest: list[tuple[int, Any]] = []
    for hi, hv in enumerate(head):
        bi = next((i for i, bv in enumerate(base) if i not in used and bv == hv), None)
        if bi is None:
            rest.append((hi, hv))
        else:
            used.add(bi)
    left = [(i, bv) for i, bv in enumerate(base) if i not in used]
    for n, (hi, hv) in enumerate(rest):
        pairs.append((str(hi), left[n][1] if n < len(left) else MISSING, hv))
    for i, bv in left[len(rest):]:
        pairs.append((f"-{i}", bv, MISSING))  # removed; "-<i>" never collides with a head index
    return pairs


def changes(base: Any, head: Any, pointer: str = "") -> list[tuple[str, Any, Any]]:
    """Every leaf that differs, as (pointer, base value or MISSING, head value or MISSING)."""
    if base == head:
        return []
    if isinstance(base, dict) and isinstance(head, dict):
        out = []
        for k in dict.fromkeys([*base, *head]):
            out += changes(base.get(k, MISSING), head.get(k, MISSING), f"{pointer}/{_esc(k)}")
        return out
    if isinstance(base, list) and isinstance(head, list):
        out = []
        for seg, b, h in _pair_list(base, head):
            out += changes(b, h, f"{pointer}/{_esc(seg)}")
        return out
    # A subtree added or removed: report each of its leaves, so a rule on a deep field still fires.
    if isinstance(base, (dict, list)) and head is MISSING:
        return [(p, b, MISSING) for p, b, _ in changes(base, {} if isinstance(base, dict) else [], pointer)] or [(pointer, base, MISSING)]
    if isinstance(head, (dict, list)) and base is MISSING:
        return [(p, MISSING, h) for p, _, h in changes({} if isinstance(head, dict) else [], head, pointer)] or [(pointer, MISSING, head)]
    return [(pointer, base, head)]


# --- tighten-only comparison -----------------------------------------------------------------

def _rank(order: Any, value: Any) -> float | None:
    """Where a value sits on the rule's order, loosest lowest. None: not on the order at all."""
    if order == "bool":
        return float(value is True) if isinstance(value, bool) else None
    if order == "int":
        return float(value) if isinstance(value, int) and not isinstance(value, bool) else None
    if isinstance(order, list):
        return float(order.index(value)) if value in order else None
    return None


def loosened(rule: dict, before: Any, after: Any) -> bool:
    default = rule.get("default", MISSING)
    b = default if before is MISSING else before
    a = default if after is MISSING else after
    rb, ra = _rank(rule["order"], b), _rank(rule["order"], a)
    if ra is None:
        return rb is not None  # an off-order value (or removing a field with no default) counts as loosening
    return rb is not None and ra < rb


# --- structural rules ------------------------------------------------------------------------

def _envs(doc: Any) -> dict[str, dict]:
    envs = ((doc or {}).get("deploy") or {}).get("environments")
    if not isinstance(envs, list):
        envs = [{"name": "dev", "tier": "ground"}]  # the schema's documented default
    return {e["name"]: e for e in envs if isinstance(e, dict) and isinstance(e.get("name"), str)}


def _steps(flow: Any) -> list[dict]:
    steps = flow.get("steps") if isinstance(flow, dict) else flow
    return [s for s in (steps or []) if isinstance(s, dict)]


def _gate_counts(doc: Any) -> dict[str, tuple[int, int]]:
    """Per flow: (check-ride steps, required test steps)."""
    out = {}
    for name, flow in (((doc or {}).get("pipelines")) or {}).items():
        rides = tests = 0
        for s in _steps(flow):
            if (s.get("stage") or s.get("task")) != "test":
                continue
            if s.get("role") == "check-ride":
                rides += 1
            elif s.get("required", True) is not False:
                tests += 1
        out[name] = (rides, tests)
    return out


def _structural(base: Any, head: Any) -> list[dict]:
    out = []
    bE, hE = _envs(base), _envs(head)
    for name in dict.fromkeys([*bE, *hE]):
        b, h = bE.get(name), hE.get(name)
        ptr = f"/deploy/environments/{_esc(name)}"
        if b and h and b.get("tier") != h.get("tier"):
            out.append(_p(ptr + "/tier", "GP-SCOPE-003", f"environment '{name}' changed tier from {b.get('tier')} to {h.get('tier')}",
                          "Moving an environment between Ground and Flight changes which gates guard it. A human makes that change."))
        elif "flight" in ((b or {}).get("tier"), (h or {}).get("tier")) and b != h and not h:
            out.append(_p(ptr, "GP-SCOPE-003", f"Flight environment '{name}' removed",
                          "Removing a Flight environment is a human's change. An agent may propose adding or changing one, "
                          "in a pull request a person merges."))
    bG, hG = _gate_counts(base), _gate_counts(head)
    for flow, (rides, tests) in bG.items():
        r2, t2 = hG.get(flow, (0, 0))
        if r2 < rides:
            out.append(_p(f"/pipelines/{_esc(flow)}", "GP-SCOPE-002", f"flow '{flow}' lost its check-ride step",
                          "The check ride is what the qa gate reads. Removing it is a human's change."))
        if t2 < tests:
            out.append(_p(f"/pipelines/{_esc(flow)}", "GP-SCOPE-002", f"flow '{flow}' has fewer required test steps ({tests} -> {t2})",
                          "Removing a required test, or setting required: false on one, loosens the flow. A human makes that change."))
    return out


def _p(pointer: str, rule: str, message: str, hint: str) -> dict:
    return {"pointer": pointer, "rule": rule, "message": message, "hint": hint}


# --- entry point -----------------------------------------------------------------------------

def scope_problems(base: Any, head: Any, scope: dict) -> list[dict]:
    """GP-SCOPE problems in the change from base to head, for an agent's pull request."""
    rules = scope["rules"]
    problems = []
    for ptr, b, h in changes(base if base is not None else {}, head if head is not None else {}):
        r = rule_for(rules, ptr)
        what = kind_of(b, h)
        if effective_scope(r, what) == "human":
            problems.append(_p(ptr, "GP-SCOPE-001", f"{ptr} {what}: a human-only field" + ("" if r else " (no rule covers it)"),
                               (r or {}).get("why") or "Ask a human to make this change, or leave the field as it was."))
        elif r["agentScope"] == "tighten" and loosened(r, b, h):
            problems.append(_p(ptr, "GP-SCOPE-002", f"{ptr} loosened ({_show(b, r)} -> {_show(h, r)})",
                               "An agent may turn a gate on or make it stricter, never the other way."))
    seen = {(p["pointer"], p["rule"]) for p in problems}
    problems += [p for p in _structural(base, head) if (p["pointer"], p["rule"]) not in seen]
    return problems


def _flight_envs(doc: Any) -> set[str]:
    return {n for n, e in _envs(doc).items() if e.get("tier") == "flight"}


def _release_steps(doc: Any) -> list[tuple[str, int, dict]]:
    out = []
    for flow, body in (((doc or {}).get("pipelines")) or {}).items():
        for i, s in enumerate(_steps(body)):
            if (s.get("stage") or s.get("task")) == "release":
                out.append((flow, i, s))
    return out


def needs_person(base: Any, head: Any, scope: dict) -> list[dict]:
    """What in the change an agent may propose but only a person may merge (the `propose` class), as dicts of
    pointer, change, message: a change to a field whose rule is propose (for that kind of change), a Flight
    environment added or changed (`/deploy/environments/<name>`), and a release step into a Flight environment added
    or changed. Empty for a change that is all open or tighten. It lists what scope_problems does not refuse; a
    change that scope_problems refuses is refused whole, whatever this lists."""
    base_doc, head_doc = base if base is not None else {}, head if head is not None else {}
    found: dict[str, dict] = {}

    def note(ptr: str, change: str, message: str) -> None:
        found.setdefault(ptr, {"pointer": ptr, "change": change, "message": message})

    for ptr, b, h in changes(base_doc, head_doc):
        r = rule_for(scope["rules"], ptr)
        kind = kind_of(b, h)
        if r is not None and r["agentScope"] == "propose" and effective_scope(r, kind) == "propose":
            note(ptr, kind, f"{ptr} {kind}: a propose field, which needs a person to merge")
    bE, hE = _envs(base_doc), _envs(head_doc)
    for name, h in hE.items():
        b = bE.get(name)
        if h.get("tier") == "flight" and b != h:
            note(f"/deploy/environments/{_esc(name)}", "added" if b is None else "changed",
                 f"Flight environment '{name}' {'added' if b is None else 'changed'}: needs a person to merge")
    flight = _flight_envs(head_doc)
    before = [s for _, _, s in _release_steps(base_doc)]
    for flow, i, s in _release_steps(head_doc):
        if s.get("env") in flight:
            if s in before:
                before.remove(s)
            else:
                note(f"/pipelines/{_esc(flow)}/steps/{i}", "added",
                     f"a release step into Flight environment '{s.get('env')}' added or changed: needs a person to merge")
    return list(found.values())


def _show(v: Any, rule: dict) -> str:
    if v is MISSING:
        v = rule.get("default", MISSING)
        return "unset" if v is MISSING else f"unset = {v!r}"
    return repr(v)
