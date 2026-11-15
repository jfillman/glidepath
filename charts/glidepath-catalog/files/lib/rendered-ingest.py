# rendered-ingest.py: how a credentialed gate reads what the render-manifests Task wrote (docs/admin/security.md,
# "Rendering is not done where the credentials are"). Spliced into a `python3 - <<'PY'` heredoc of the `ingest` step of
# policy-validation-gate and clearance-gate by glidepath-catalog.renderedIngest, so both read it exactly the same way.
#
# The rendered tree is ATTACKER-CONTROLLED DATA: a pull request author chooses the chart and the values. This reads it
# and nothing else; it never executes, sources or evals any of it, never follows a symlink in it, and never prints any of
# it: a log line carries an environment's id (checked against a strict pattern first) and counts, nothing from a file.
#
#   /work/plan.txt       the environments, from the trusted fetch step (cluster|env|chart[|protected])
#   /rendered/           the render task's output, mounted read-only:
#                          MANIFEST                  "<sha256>  <environment id>/<file>" per file, written last
#                          <cluster>__<env>/status   ok | render-failed
#                          <cluster>__<env>/head.yaml, base.yaml    (clearance-gate)
#                          <cluster>__<env>/resources.yaml          (policy-validation)
#   /work/eval/<id>/     what this writes, from the bytes it read: the files below the old render step wrote, so the steps
#                        after it are unchanged (head.yaml, base.yaml, resources.yaml, env, status)
#
# What it refuses (exit 1, the check fails): anything in the tree that is not a plain file or a directory (a symlink, a device,
# a pipe), a name outside the expected shape, a file the manifest does not list or one it lists that is missing or differs, a
# file over the size limit or a tree over the total, a document with a tag (!!python/object, anything but YAML's core tags),
# a recursive anchor, nesting too deep, too many documents, or more nodes once aliases are expanded than the limit (an alias
# bomb). The limits are the RENDER_* environment variables below. A parse that passes is then built with the SafeLoader, which
# constructs only plain data.
import hashlib, os, re, stat, sys, yaml

WORK, RENDERED = "/work", "/rendered"
GATE = os.environ.get("GATE", "")


def limit(name, default):
    try:
        return int(os.environ.get(name) or default)
    except ValueError:
        return default


MAX_FILE = limit("RENDER_MAX_FILE_BYTES", 32 * 1024 * 1024)
MAX_TOTAL = limit("RENDER_MAX_TOTAL_BYTES", 128 * 1024 * 1024)
MAX_DOCS = limit("RENDER_MAX_DOCUMENTS", 5000)
MAX_DEPTH = limit("RENDER_MAX_DEPTH", 64)
MAX_NODES = limit("RENDER_MAX_NODES", 1000000)
MAX_ENVS = limit("RENDER_MAX_ENVIRONMENTS", 50)

FILES = {"clearance-gate": ("head.yaml", "base.yaml"), "policy-validation": ("resources.yaml",), "values": ("resources.yaml",)}
if GATE not in FILES:
    sys.exit("rendered-ingest: GATE must be clearance-gate, policy-validation or values")
YAML_FILES = FILES[GATE]
EXTRA_FILES = ("released",) if GATE == "values" else ()   # the values gate's one flag file: read as a word, not parsed
SAFE_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
CORE_TAGS = {"tag:yaml.org,2002:" + t for t in ("str", "int", "float", "bool", "null", "map", "seq", "timestamp", "binary", "set", "omap", "pairs", "merge")}


def shown(s):
    """Anything printed that came from a name: only safe characters, bounded."""
    return re.sub(r"[^A-Za-z0-9._/-]", "?", str(s))[:80]


def fail(msg):
    print("FAIL: " + msg, file=sys.stderr)
    sys.exit(1)


def read_file(path, budget):
    """The bytes of a regular file, opened without following a symlink; refuses anything else."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        fail("the rendered tree has an entry that is not a plain file")
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            fail("the rendered tree has an entry that is not a plain file")
        if st.st_size > MAX_FILE:
            fail("a rendered file is larger than the limit of %d bytes" % MAX_FILE)
        chunks, got = [], 0
        while True:
            b = os.read(fd, 1 << 20)
            if not b:
                break
            got += len(b)
            if got > MAX_FILE:
                fail("a rendered file is larger than the limit of %d bytes" % MAX_FILE)
            chunks.append(b)
    finally:
        os.close(fd)
    if budget[0] - got < 0:
        fail("the rendered tree is larger than the limit of %d bytes" % MAX_TOTAL)
    budget[0] -= got
    return b"".join(chunks)


def scan():
    """-> ({relative path: bytes}, manifest bytes). Reads each file once; the digest and the parse are of these bytes."""
    try:
        top = os.lstat(RENDERED)
    except OSError:
        fail("the render task's output is not there")
    if not stat.S_ISDIR(top.st_mode):
        fail("the render task's output is not a directory")
    budget, files, manifest = [MAX_TOTAL], {}, None
    for name in sorted(os.listdir(RENDERED)):
        p = os.path.join(RENDERED, name)
        st = os.lstat(p)
        if name == "MANIFEST":
            manifest = read_file(p, budget)
        elif stat.S_ISDIR(st.st_mode) and SAFE_NAME.match(name):
            for f in sorted(os.listdir(p)):
                if f not in YAML_FILES + EXTRA_FILES + ("status",):
                    fail("the rendered tree has a file this gate does not read")
                files[name + "/" + f] = read_file(os.path.join(p, f), budget)
        else:
            fail("the rendered tree has an entry that is not an environment's directory")
    if manifest is None:
        fail("the render task wrote no MANIFEST (it did not finish)")
    return files, manifest


def verify_manifest(files, manifest):
    listed = {}
    try:
        lines = manifest.decode("utf-8").split("\n")
    except UnicodeDecodeError:
        fail("the MANIFEST is not text")
    for line in lines:
        if not line:
            continue
        m = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+)", line)
        if not m or m.group(2) in listed:
            fail("the MANIFEST has a line that is not '<sha256>  <environment>/<file>', or lists a file twice")
        listed[m.group(2)] = m.group(1)
    if set(listed) != set(files):
        fail("the MANIFEST does not list exactly the files in the rendered tree")
    for path, data in files.items():
        if hashlib.sha256(data).hexdigest() != listed[path]:
            fail("a rendered file does not match its digest in the MANIFEST")


def inspect_node(node, depth, memo, stack):
    """-> (expanded node count, height) of a composed YAML node, aliases expanded as a constructor would."""
    if depth > MAX_DEPTH:
        fail("a rendered document is nested more than %d levels deep" % MAX_DEPTH)
    nid = id(node)
    if nid in stack:
        fail("a rendered document has a recursive anchor")
    if nid in memo:
        size, height = memo[nid]
        if depth + height - 1 > MAX_DEPTH:
            fail("a rendered document is nested more than %d levels deep" % MAX_DEPTH)
        return memo[nid]
    if node.tag not in CORE_TAGS:
        fail("a rendered document carries a YAML tag other than the core ones")
    stack.add(nid)
    size, height = 1, 1
    if isinstance(node, yaml.SequenceNode):
        kids = list(node.value)
    elif isinstance(node, yaml.MappingNode):
        kids = [x for kv in node.value for x in kv]
    else:
        kids = []
    for kid in kids:
        s, h = inspect_node(kid, depth + 1, memo, stack)
        size += s
        height = max(height, h + 1)
        if size > MAX_NODES:
            fail("a rendered document expands to more than %d nodes once its aliases are expanded" % MAX_NODES)
    stack.discard(nid)
    memo[nid] = (size, height)
    return memo[nid]


def parse(data):
    """The documents of a rendered YAML file as plain data, after the bounds above; refuses on any violation."""
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        fail("a rendered file is not UTF-8")
    expanded, docs = 0, 0
    try:
        for node in yaml.compose_all(text, Loader=yaml.SafeLoader):
            docs += 1
            if docs > MAX_DOCS:
                fail("a rendered file has more than %d documents" % MAX_DOCS)
            if node is not None:
                size, _ = inspect_node(node, 1, {}, set())
                expanded += size
                if expanded > MAX_NODES:
                    fail("a rendered file expands to more than %d nodes once its aliases are expanded" % MAX_NODES)
        return [d for d in yaml.safe_load_all(text)]
    except RecursionError:
        fail("a rendered document is nested too deeply")
    except Exception:
        # YAMLError, but also what a scalar's constructor raises (a timestamp such as 2023-13-45, !!float abc, !!int x, a bad !!binary):
        # one fixed message, never the exception's text, which quotes the repository's string
        fail("a rendered file is not valid YAML (or builds something other than plain data)")


def objects(documents):
    """The Kubernetes objects among the documents: mappings with a kind and a metadata mapping (what the gates always counted)."""
    return [d for d in documents if isinstance(d, dict) and d.get("kind") and isinstance(d.get("metadata"), dict)]


def write(path, data):
    with open(path, "wb") as f:
        f.write(data)


def main():
    plan = []
    if os.path.exists(WORK + "/plan.txt"):
        for line in open(WORK + "/plan.txt", encoding="utf-8", errors="replace").read().split("\n"):
            if line:
                plan.append(line.split("|"))
    if len(plan) > MAX_ENVS:
        fail("the plan names %d environments; the limit is %d" % (len(plan), MAX_ENVS))
    files, manifest = scan()
    verify_manifest(files, manifest)
    # a directory the plan does not name: the render task wrote something this run did not ask for
    planned = {row[0] + "__" + row[1] for row in plan if len(row) >= 2}
    if {p.split("/")[0] for p in files} - planned:
        fail("the rendered tree has an environment this run's plan does not name")
    # Everything is decided before anything is written: a refused tree leaves nothing behind for the steps after this one.
    actions, failed = [], 0
    for row in plan:
        want = 4 if GATE == "clearance-gate" else 3
        if len(row) != want:
            fail("a line of the plan is not in the shape this gate reads")
        cluster, env, protected = row[0], row[1], (row[3] if GATE == "clearance-gate" else "true")
        label = shown(cluster) + "/" + shown(env)
        if not (SAFE_NAME.match(cluster) and SAFE_NAME.match(env)):
            print("FAIL %s: the cluster or environment name is not a plain name" % label)
            failed += 1
            continue
        eid = cluster + "__" + env
        if protected != "true":
            print("%s: not a protected environment (no production: true): skipped" % label)
            actions.append((eid, b"skipped\n", None, None, label))
            continue
        if os.path.exists("%s/observability/%s.yaml.unreadable" % (WORK, cluster)):
            print("FAIL %s: %s's observability facts could not be read (the broker's /policy-bundle), so the render would differ from Argo CD's" % (label, shown(cluster)))
            actions.append((eid, b"render-failed\n", None, None, label))
            failed += 1
            continue
        if files.get(eid + "/status", b"").strip() != b"ok" or any((eid + "/" + y) not in files for y in YAML_FILES):
            print("FAIL %s: the render task did not render this environment (its log says why)" % label)
            actions.append((eid, b"render-failed\n", None, None, label))
            failed += 1
            continue
        parsed = {y: parse(files[eid + "/" + y]) for y in YAML_FILES}
        if GATE == "values":
            # the two things a render can prove about an own chart (docs/admin/chart-contract.md): a Namespace, and once released a Rollout named after the app
            objs = objects(parsed["resources.yaml"])
            kinds = {(d["kind"], d["metadata"].get("name")) for d in objs}
            app = os.environ.get("APP_NAME", "")
            released = files.get(eid + "/released", b"").strip() == b"true"
            missing = []
            if not any(k == "Namespace" for k, _ in kinds):
                missing.append("a Namespace")
            if released and ("Rollout", app) not in kinds:
                missing.append("a Rollout named " + shown(app))
            if missing:
                print("FAIL %s: rendered %d objects but no %s (docs/admin/chart-contract.md)" % (label, len(objs), " or ".join(missing)))
                failed += 1
            else:
                print("ok   %s: %d objects" % (label, len(objs)))
            continue
        actions.append((eid, None, {y: files[eid + "/" + y] for y in YAML_FILES}, parsed, label, cluster, env))
    os.makedirs(WORK + "/eval", exist_ok=True)
    for a in actions:
        eid, status, data, parsed, label = a[:5]
        out = WORK + "/eval/" + eid
        os.makedirs(out, exist_ok=True)
        if status is not None:
            write(out + "/status", status)
            continue
        for y in YAML_FILES:
            write(out + "/" + y, data[y])  # the bytes that were read, verified and parsed
        if GATE == "clearance-gate":
            head, base = len(objects(parsed["head.yaml"])), len(objects(parsed["base.yaml"]))
            write(out + "/env", (a[5] + "\n" + a[6] + "\n").encode())
            if os.path.lexists(out + "/resources.yaml"):
                os.remove(out + "/resources.yaml")
            os.symlink("head.yaml", out + "/resources.yaml")  # what the admission evaluation reads (policy-eval-prepare.py)
            print("%s: read the rendered head (%d objects) and base (%d objects)%s" % (label, head, base, "" if len(data["base.yaml"]) else "; new in this PR (no base tree)"))
        else:
            print("%s: read %d rendered objects" % (label, len(objects(parsed["resources.yaml"]))))
    sys.exit(1 if failed else 0)


main()
