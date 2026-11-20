#!/usr/bin/env python3
"""Every chart's values.yaml has no duplicate mapping key. YAML allows one and Helm silently keeps the last, so a second top-level
`itsm:` would drop the first block's keys without any error. Run: python3 charts/glidepath-control-plane/tests/values_unique_keys_test.py (needs pyyaml)"""
import glob
import os
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


class UniqueKeyLoader(yaml.SafeLoader):
    def construct_mapping(self, node, deep=False):
        seen = set()
        for k, _ in node.value:
            key = self.construct_object(k, deep=True)
            if key in seen:
                raise yaml.constructor.ConstructorError(None, None, "duplicate key %r" % (key,), k.start_mark)
            seen.add(key)
        return super().construct_mapping(node, deep)


class ValuesFiles(unittest.TestCase):
    def test_no_values_file_has_a_duplicate_key(self):
        files = sorted(glob.glob(os.path.join(ROOT, "charts", "*", "values*.yaml")) + glob.glob(os.path.join(ROOT, "charts", "*", "tests", "values*.yaml")))
        self.assertGreaterEqual(len(files), 4, "the glob found too few files: the check would be vacuous")
        for f in files:
            with open(f) as fh:
                try:
                    yaml.load(fh, Loader=UniqueKeyLoader)
                except yaml.YAMLError as e:
                    self.fail("%s: %s" % (os.path.relpath(f, ROOT), e))

    def test_the_loader_rejects_a_duplicate(self):
        with self.assertRaises(yaml.YAMLError):
            yaml.load("a: 1\nb: 2\na: 3\n", Loader=UniqueKeyLoader)


if __name__ == "__main__":
    unittest.main()
