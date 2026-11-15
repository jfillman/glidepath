#!/usr/bin/env python3
"""No `cmd | grep -q ...` in a task script, a script library or an operator script, unless it says why it is safe.

Under `set -o pipefail`, `grep -q` (and `grep -m1`) quits at the first match. If the command feeding it is still writing, that
command gets SIGPIPE (status 141), the pipeline's status is non-zero, and an `if` or `&&` reads the match as "no". 2026-11-15, on
the lab: onboarding-resync's `git status --porcelain | grep -q .` answered "nothing changed" for a repository eleven files behind,
twice, and no resync PR opened (docs/admin/onboarding-mechanics.md, "A resync that says nothing to deliver").

Decide from a value instead: capture the producer's output first (`out="$(cmd)"`, a failing command stops the script under set -e),
then test the variable (`[[ -n "${out}" ]]`, `[[ "${out}" == *x* ]]`, `grep -q x <<<"${out}"`, which is not a pipe).

A pipe that is safe, because the producer writes less than a pipe buffer in one go and cannot be cut off, or because the branch is
not a decision, stays only with a reason on the same line or the line above:

    # pipe-ok: <why>

Run:  python3 charts/glidepath-catalog/tests/pipefail_early_exit_test.py
"""
import os
import re
import unittest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
SCAN = ["charts", "catalog", "hack"]
SKIP_DIRS = {"tests", "__pycache__", "node_modules", ".git"}
SUFFIXES = (".yaml", ".yml", ".tpl", ".sh", ".py")

# `| grep` whose options include -q (alone or bundled: -qE, -Eq, -qxF), --quiet/--silent, or -m N / --max-count.
EARLY = re.compile(
    r"(?<!\|)\|(?!\|)\s*(?:\w+=\S+\s+)*grep\s+(?:[^|;&<>]*\s)?(?:-[A-Za-z]*q[A-Za-z]*|--quiet|--silent|-m\s*\d+|--max-count)(?=\s|=|$)"
)
PIPE_OK = re.compile(r"#\s*pipe-ok:\s*\S+")


def files():
    for top in SCAN:
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, top)):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for n in filenames:
                if n.endswith(SUFFIXES):
                    yield os.path.join(dirpath, n)


def findings(text):
    lines = text.splitlines()
    out = []
    for i, line in enumerate(lines):
        code = line.strip()
        if code.startswith("#"):
            continue
        if not EARLY.search(line):
            continue
        if PIPE_OK.search(line) or (i > 0 and PIPE_OK.search(lines[i - 1])):
            continue
        out.append((i + 1, code[:160]))
    return out


class PipefailEarlyExit(unittest.TestCase):
    def test_no_unannotated_pipe_into_grep_q(self):
        bad = []
        for f in files():
            with open(f, encoding="utf-8") as fh:
                for n, code in findings(fh.read()):
                    bad.append(f"{os.path.relpath(f, ROOT)}:{n}: {code}")
        self.assertEqual(bad, [], "a pipe into `grep -q` under pipefail reads SIGPIPE as 'no match'; capture the output first, or annotate "
                                  "`# pipe-ok: <reason>`:\n  " + "\n  ".join(bad))

    def test_detector_flags_what_it_should(self):
        flagged = [
            "if git status --porcelain | grep -q .; then",
            "  | grep -qxF \"${x}\"; then",
            "x | grep -E -q 'a|b'",
            "x | grep -qE 'a'",
            "x | grep --quiet a",
            "x | grep -m1 a && y",
            "x | grep -Eq a",
        ]
        for line in flagged:
            self.assertTrue(findings(line), line)
        fine = [
            "grep -q x <<<\"${out}\"",
            "x | grep -E 'a'",
            "x | grep -c .",
            "# if git status | grep -q .",
            "x | grep -q a  # pipe-ok: one short line, written whole",
            "git status | grep -oE 'q'",
        ]
        for line in fine:
            self.assertFalse(findings(line), line)
        self.assertFalse(findings("# pipe-ok: a single write\nx | grep -q a"))


if __name__ == "__main__":
    unittest.main()
