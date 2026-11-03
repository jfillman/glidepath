# policy-eval-prepare.py: the Kyverno CLI's inputs for an offline admission evaluation (glidepath
# docs/admin/policy-validation.md, ADR-0028), shared by the policy-validation-gate and clearance-gate Tasks (spliced
# into a `python3 - <<'PY'` heredoc of their render step by glidepath-catalog.policyEvalPrepare, so both run exactly
# this). Reads every /work/eval/<cluster>__<env>/resources.yaml without a status file and every /work/policies/<cluster>
# bundle; writes crds.yaml and values.yaml next to each environment's resources and <cluster>.bundle.yaml +
# <cluster>.index.json next to each bundle. Needs python3 + pyyaml and Airframe's XRDs under /opt/airframe/xrds (the
# validator image).
import glob, json, os, sys, yaml

def docs(path):
    try:
        return [d for d in yaml.safe_load_all(open(path)) if isinstance(d, dict)]
    except yaml.YAMLError as e:
        print(f"warning: {path} is not valid YAML, ignored: {e}", file=sys.stderr)
        return []

# Plurals the cluster really uses for Airframe's kinds; anything else gets the API server's own guess
# (k8s.io/apimachinery UnsafeGuessKindToResource), which is what a CRD without explicit names would get.
xrd_plural = {}
for f in glob.glob("/opt/airframe/xrds/*.yaml"):
    for d in docs(f):
        if d.get("kind") == "CompositeResourceDefinition":
            n = d["spec"]["names"]
            xrd_plural[(d["spec"]["group"], n["kind"])] = n["plural"]

def guess(kind):
    k = kind.lower()
    if k.endswith(("s", "x", "z", "ch", "sh")):
        return k + "es"
    if k.endswith("y") and len(k) > 1 and k[-2] not in "aeiou":
        return k[:-1] + "ies"
    return k + "s"

BUILTIN = {"", "v1", "apps", "batch", "autoscaling", "policy"}
for out in sorted(glob.glob("/work/eval/*")):
    if os.path.exists(f"{out}/status"):
        continue
    seen, namespaces = {}, []
    for d in docs(f"{out}/resources.yaml"):
        av, kind = d.get("apiVersion", ""), d.get("kind", "")
        if kind == "Namespace":
            labels = dict((d.get("metadata") or {}).get("labels") or {})
            labels["kubernetes.io/metadata.name"] = d["metadata"]["name"]  # set by the API server
            namespaces.append({"name": d["metadata"]["name"], "labels": labels})
        group, _, version = av.rpartition("/")
        if group in BUILTIN or group.endswith(".k8s.io"):
            continue
        seen.setdefault((group, kind), set()).add(version)
    crds = []
    for (group, kind), versions in sorted(seen.items()):
        plural = xrd_plural.get((group, kind)) or guess(kind)
        crds.append({"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
                     "metadata": {"name": f"{plural}.{group}"},
                     "spec": {"group": group, "scope": "Namespaced",
                              "names": {"kind": kind, "plural": plural, "singular": kind.lower(), "listKind": kind + "List"},
                              "versions": [{"name": v, "served": True, "storage": i == 0,
                                            "schema": {"openAPIV3Schema": {"type": "object", "x-kubernetes-preserve-unknown-fields": True}}}
                                           for i, v in enumerate(sorted(versions))]}})
    with open(f"{out}/crds.yaml", "w") as f:
        yaml.safe_dump_all(crds, f)
    with open(f"{out}/values.yaml", "w") as f:
        yaml.safe_dump({"apiVersion": "cli.kyverno.io/v1alpha1", "kind": "Values", "namespaceSelector": namespaces}, f)

# The bundle, normalized for an offline run (the header says why). index.json maps each evaluated policy name
# to its source object and its blocking mode: deny (the cluster refuses) or warn (Audit/Warn only).
for cdir in sorted(d for d in glob.glob("/work/policies/*") if os.path.isdir(d)):
    all_docs = [d for f in sorted(glob.glob(f"{cdir}/**/*.y*ml", recursive=True)) for d in docs(f)]
    vaps = {d["metadata"]["name"]: d for d in all_docs if d.get("kind") == "ValidatingAdmissionPolicy"}
    bundle, index = [], {}
    for d in all_docs:
        kind, name = d.get("kind"), (d.get("metadata") or {}).get("name", "")
        if kind in ("ValidatingPolicy", "NamespacedValidatingPolicy") and d.get("apiVersion", "").startswith("policies.kyverno.io/"):
            d["spec"].setdefault("evaluation", {}).setdefault("background", {})["enabled"] = True
            actions = d["spec"].get("validationActions") or ["Deny"]
            index[name] = {"source": f"{kind}/{name}", "mode": "deny" if "Deny" in actions else "warn"}
            bundle.append(d)
        elif kind in ("ClusterPolicy", "Policy") and d.get("apiVersion", "").startswith("kyverno.io/"):
            spec = d.get("spec") or {}
            spec["background"] = True
            rules = [r for r in spec.get("rules") or [] if "validate" in r]
            if not rules:
                continue
            modes = [((r["validate"].get("failureAction") or spec.get("validationFailureAction") or "Audit").lower()) for r in rules]
            index[name] = {"source": f"{kind}/{name}", "mode": "deny" if "enforce" in modes else "warn"}
            bundle.append(d)
        elif kind == "ValidatingAdmissionPolicyBinding":
            p = vaps.get((d.get("spec") or {}).get("policyName"))
            if not p:
                continue
            if p["spec"].get("paramKind"):
                print(f"note: {name}: ValidatingAdmissionPolicy {p['metadata']['name']} takes parameters, not evaluated offline", file=sys.stderr)
                continue
            spec = {k: v for k, v in p["spec"].items()}
            actions = d["spec"].get("validationActions") or ["Deny"]
            spec["validationActions"] = actions
            spec["evaluation"] = {"background": {"enabled": True}}
            # The binding narrows where the policy applies; both must match (selectors only, which is what
            # Apron's bindings use).
            mr = d["spec"].get("matchResources") or {}
            mc = spec.setdefault("matchConstraints", {})
            for sel in ("namespaceSelector", "objectSelector"):
                if sel in mr and sel not in mc:
                    mc[sel] = mr[sel]
            bundle.append({"apiVersion": "policies.kyverno.io/v1", "kind": "ValidatingPolicy",
                           "metadata": {"name": name}, "spec": spec})
            index[name] = {"source": f"ValidatingAdmissionPolicy/{p['metadata']['name']} (binding {name})",
                           "mode": "deny" if "Deny" in actions else "warn"}
    with open(f"{cdir}.bundle.yaml", "w") as f:
        yaml.safe_dump_all(bundle, f)
    with open(f"{cdir}.index.json", "w") as f:
        json.dump(index, f)
    print(f"{os.path.basename(cdir)}: {len(bundle)} admission polic{'y' if len(bundle) == 1 else 'ies'} to evaluate: {', '.join(sorted(index)) or 'none'}")
