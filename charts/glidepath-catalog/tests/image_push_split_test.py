#!/usr/bin/env python3
"""build-image / push-image split (docs/admin/security.md, "Pipeline identities", "Image build and push").

The image build runs the repository's Containerfile `RUN` lines. It used to do that in the container that mounted the registry
push credential, so a pull request could read the credential and push to any repository it reached. Now:

  build-image  runs as source-runner, mounts no registry push credential (at most an optional READ-ONLY pull credential), builds to
               an OCI layout in the workspace and pushes nothing. Its results are untrusted data.
  push-image   a separate pod, a credentialed account, runs nothing from the repository. It validates the layout structurally,
               then copies it with oras (which does not execute content) to the repository the platform's parameters name,
               and reports the digest it pushed.

This file checks (1) the rendered accounts, volumes and results of both tasks, (2) the layout validator against hostile layouts,
(3) the push step end to end with a fake oras (what it pushes, where, and what it refuses), (4) that a pull request's build can
neither export a registry cache nor write the cache a main build imports, and (5) that nothing downstream takes an image
reference or digest from the build task.

Needs helm, pyyaml, bash, jq.   Run:  python3 charts/glidepath-catalog/tests/image_push_split_test.py
"""
import hashlib
import json
import os
import re
import stat
import subprocess
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
CATALOG = os.path.join(ROOT, "charts", "glidepath-catalog")
LIB = os.path.join(CATALOG, "files", "lib", "oci-layout.sh")


def render(*extra):
    r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"), *extra],
                       capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    return [d for d in yaml.safe_load_all(r.stdout) if d]


DOCS = render()
TASKS = {d["metadata"]["name"]: d for d in DOCS if d["kind"] == "Task"}
PIPELINES = {d["metadata"]["name"]: d for d in DOCS if d["kind"] == "Pipeline"}
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
CACHE_CONFIG = "application/vnd.buildkit.cacheconfig.v0"
LAYER_GZ = "application/vnd.oci.image.layer.v1.tar+gzip"
SHA = "b01565a54f78f0f28d13ec2a2fca114b3f9b3569"


def step(task, name):
    return [s for s in TASKS[task]["spec"]["steps"] if s["name"] == name][0]


def executable(path, body):
    with open(path, "w") as f:
        f.write("#!/bin/bash\n" + body + "\n")
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)


# ------------------------------------------------------------------ layout fixtures
def sha(data):
    return hashlib.sha256(data).hexdigest()


def put(root, data):
    os.makedirs(os.path.join(root, "blobs", "sha256"), exist_ok=True)
    h = sha(data)
    with open(os.path.join(root, "blobs", "sha256", h), "wb") as f:
        f.write(data)
    return h


def desc(media, data, **extra):
    return dict({"mediaType": media, "digest": "sha256:" + sha(data), "size": len(data)}, **extra)


def jb(obj):
    return json.dumps(obj, sort_keys=True).encode()


def image_manifest(root, arch, layer_media=LAYER_GZ, layers=1, config_media=OCI_CONFIG, cfg_arch=None, layer_extra=None):
    cfg = jb({"architecture": cfg_arch or arch, "os": "linux", "rootfs": {"type": "layers", "diff_ids": []}})
    put(root, cfg)
    ls = []
    for i in range(layers):
        data = (b"layer-%d-" % i) + arch.encode() + b"\0" * 64
        put(root, data)
        ls.append(desc(layer_media, data, **(layer_extra or {})))
    m = jb({"schemaVersion": 2, "mediaType": OCI_MANIFEST, "config": desc(config_media, cfg), "layers": ls})
    put(root, m)
    return m


def write_top(root, top_desc_obj):
    with open(os.path.join(root, "oci-layout"), "w") as f:
        json.dump({"imageLayoutVersion": "1.0.0"}, f)
    with open(os.path.join(root, "index.json"), "w") as f:
        json.dump({"schemaVersion": 2, "manifests": [top_desc_obj]}, f)


def make_image(root, platforms=("linux/amd64", "linux/arm64"), **kw):
    """-> the digest of the top descriptor (the index for several platforms, the manifest for one)."""
    entries = []
    force_index = kw.pop("force_index", False)
    for p in platforms:
        os_, arch = p.split("/")
        m = image_manifest(root, arch, **kw)
        entries.append(desc(OCI_MANIFEST, m, platform={"os": os_, "architecture": arch}))
    if len(platforms) == 1 and not force_index:
        top = {"mediaType": OCI_MANIFEST, "digest": entries[0]["digest"], "size": entries[0]["size"]}
    else:
        idx = jb({"schemaVersion": 2, "mediaType": OCI_INDEX, "manifests": entries})
        put(root, idx)
        top = desc(OCI_INDEX, idx)
    write_top(root, top)
    return top["digest"]


def make_raw_image(root, build_manifest_text, platform="linux/amd64"):
    """A single-platform image whose manifest is the text build_manifest_text(config_desc_json, layer_desc_json) returns, so a test
    can write keys a json.dumps never would (duplicates, other spellings)."""
    arch = platform.split("/")[1]
    cfg = jb({"architecture": arch, "os": "linux", "rootfs": {"type": "layers", "diff_ids": []}})
    put(root, cfg)
    layer = b"layer-0-" + arch.encode() + b"\0" * 64
    put(root, layer)
    text = build_manifest_text(json.dumps(desc(OCI_CONFIG, cfg)), json.dumps(desc(LAYER_GZ, layer))).encode()
    put(root, text)
    top = desc(OCI_MANIFEST, text)
    write_top(root, top)
    return top["digest"]


EVIL_LAYER = '{"mediaType": "text/html", "digest": "sha256:' + "0" * 64 + '", "size": 1, "urls": ["http://evil.example/x"]}'


def make_cache(root, config_media=CACHE_CONFIG, layer_media=LAYER_GZ, layers=1):
    cfg = jb({"layers": [], "records": []})
    put(root, cfg)
    ls = []
    for i in range(layers):
        data = b"cache-layer-%d-" % i + b"\1" * 32
        put(root, data)
        ls.append(desc(layer_media, data))
    m = jb({"schemaVersion": 2, "mediaType": OCI_MANIFEST, "config": desc(config_media, cfg), "layers": ls})
    put(root, m)
    top = desc(OCI_MANIFEST, m)
    write_top(root, top)
    return top["digest"]


def blob_path(root, digest):
    return os.path.join(root, "blobs", "sha256", digest.split(":")[1])


def rewrite_json(path, fn):
    with open(path) as f:
        o = json.load(f)
    fn(o)
    with open(path, "w") as f:
        json.dump(o, f)


def run_validate(root, kind="image", want="linux/amd64,linux/arm64", env=None):
    script = 'source "%s"; oci_layout_validate "%s" "%s" "%s" && printf "OK %%s\\n" "${OCI_LAYOUT_DIGEST}"' % (LIB, root, kind, want)
    p = subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=dict(os.environ, **(env or {})))
    return p.returncode, p.stdout, p.stderr


class LayoutValidator(unittest.TestCase):
    def check(self, build, kind="image", want="linux/amd64,linux/arm64", env=None):
        with tempfile.TemporaryDirectory() as d:
            root = os.path.join(d, "layout")
            os.makedirs(root)
            digest = build(root, d)
            code, out, err = run_validate(root, kind, want, env)
            return code, out, err, digest

    def refused(self, build, why, **kw):
        code, out, err, _ = self.check(build, **kw)
        self.assertNotEqual(code, 0, "expected a refusal (%s), got: %s %s" % (why, out, err))
        self.assertNotIn("OK ", out)
        self.assertIn("refusing", err)
        return err

    # ---- what is accepted
    def test_a_multi_platform_layout_is_accepted_and_reports_the_index_digest(self):
        code, out, err, digest = self.check(lambda r, d: make_image(r))
        self.assertEqual(code, 0, err)
        self.assertEqual(out.strip(), "OK " + digest)

    def test_a_single_platform_layout_is_accepted_and_reports_the_manifest_digest(self):
        code, out, err, digest = self.check(lambda r, d: make_image(r, ("linux/amd64",)), want="linux/amd64")
        self.assertEqual(code, 0, err)
        self.assertEqual(out.strip(), "OK " + digest)

    def test_a_cache_layout_is_accepted(self):
        code, out, err, digest = self.check(lambda r, d: make_cache(r), kind="cache", want="")
        self.assertEqual(code, 0, err)
        self.assertEqual(out.strip(), "OK " + digest)

    # ---- symlinks, traversal, odd files
    def test_a_symlinked_blob_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            secret = os.path.join(d, "token")
            open(secret, "w").write("SECRETTOKEN")
            layer = [p for p in os.listdir(os.path.join(r, "blobs", "sha256"))][0]
            os.remove(os.path.join(r, "blobs", "sha256", layer))
            os.symlink(secret, os.path.join(r, "blobs", "sha256", layer))
            return digest
        err = self.refused(build, "symlink")
        self.assertNotIn("SECRETTOKEN", err)

    def test_a_symlinked_directory_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            other = os.path.join(d, "elsewhere")
            os.rename(os.path.join(r, "blobs"), other)
            os.symlink(other, os.path.join(r, "blobs"))
            return digest
        self.refused(build, "symlinked blobs dir")

    def test_a_symlinked_index_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            real = os.path.join(d, "index.json")
            os.rename(os.path.join(r, "index.json"), real)
            os.symlink(real, os.path.join(r, "index.json"))
            return digest
        self.refused(build, "symlinked index")

    def test_the_layout_directory_itself_may_not_be_a_symlink(self):
        with tempfile.TemporaryDirectory() as d:
            real = os.path.join(d, "real")
            os.makedirs(real)
            make_image(real)
            link = os.path.join(d, "link")
            os.symlink(real, link)
            code, out, err = run_validate(link)
            self.assertNotEqual(code, 0)
            self.assertIn("refusing", err)

    def test_a_missing_layout_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            code, out, err = run_validate(os.path.join(d, "nope"))
            self.assertNotEqual(code, 0)
            self.assertIn("refusing", err)

    def test_an_entry_with_an_unexpected_name_is_refused(self):
        for name in ("extra.txt", ".hidden", "blobs/sha256/not-a-digest", "blobs/sha256/..%2f..%2fetc", "blobs/other/" + "a" * 64):
            def build(r, d, name=name):
                digest = make_image(r)
                p = os.path.join(r, name)
                os.makedirs(os.path.dirname(p), exist_ok=True)
                open(p, "w").write("x")
                return digest
            self.refused(build, name)

    def test_a_fifo_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            os.mkfifo(os.path.join(r, "blobs", "sha256", "f" * 64))
            return digest
        self.refused(build, "fifo")

    def test_a_blob_digest_that_climbs_out_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            rewrite_json(os.path.join(r, "index.json"), lambda o: o["manifests"][0].update(digest="sha256:../../../../etc/passwd"))
            return digest
        self.refused(build, "traversal in a digest")

    # ---- integrity and size
    def test_a_blob_whose_content_does_not_match_its_digest_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            layer = [m for m in os.listdir(os.path.join(r, "blobs", "sha256"))
                     if open(os.path.join(r, "blobs", "sha256", m), "rb").read().startswith(b"layer-0-arm64")][0]
            open(os.path.join(r, "blobs", "sha256", layer), "ab").write(b"x")
            return digest
        self.refused(build, "tampered blob")

    def test_a_blob_whose_size_does_not_match_its_descriptor_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            idx = json.load(open(os.path.join(r, "index.json")))["manifests"][0]
            rewrite_json(os.path.join(r, "index.json"), lambda o: o["manifests"][0].update(size=idx["size"] + 1))
            return digest
        self.refused(build, "size lies")

    def test_a_missing_blob_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            os.remove(blob_path(r, json.load(open(os.path.join(r, "index.json")))["manifests"][0]["digest"]))
            return digest
        self.refused(build, "missing index blob")

    def test_an_oversized_layer_is_refused(self):
        err = self.refused(lambda r, d: make_image(r), "over the total cap", env={"OCI_MAX_TOTAL_BYTES": "100"})
        self.assertIn("bytes", err)

    def test_an_oversized_index_json_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            rewrite_json(os.path.join(r, "index.json"), lambda o: o.update(annotations={"x": "y" * 2_000_000}))
            return digest
        self.refused(build, "big index.json")

    def test_too_many_layers_are_refused(self):
        self.refused(lambda r, d: make_image(r, ("linux/amd64",), layers=5), "layer count", want="linux/amd64", env={"OCI_MAX_LAYERS": "4"})

    # ---- media types
    def test_an_unexpected_layer_media_type_is_refused(self):
        for media in ("application/vnd.docker.image.rootfs.foreign.diff.tar.gzip", "text/html", "application/x-sh", ""):
            self.refused(lambda r, d, media=media: make_image(r, layer_media=media), media)

    def test_an_unexpected_config_media_type_is_refused(self):
        self.refused(lambda r, d: make_image(r, config_media="application/x-sh"), "config type")
        # a cache config is not an image config, and the other way round
        self.refused(lambda r, d: make_image(r, config_media=CACHE_CONFIG), "cache config as image config")
        self.refused(lambda r, d: make_cache(r, config_media=OCI_CONFIG), "image config as cache config", kind="cache", want="")

    def test_an_unexpected_top_media_type_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            rewrite_json(os.path.join(r, "index.json"), lambda o: o["manifests"][0].update(mediaType="application/x-sh"))
            return digest
        self.refused(build, "top type")

    def test_a_foreign_layer_with_urls_is_refused(self):
        self.refused(lambda r, d: make_image(r, layer_extra={"urls": ["http://evil.example/layer"]}), "urls")

    # ---- parser differential: jq is case-sensitive and last-wins, Go's encoding/json (oras, registries) folds case
    def raw(self, build):
        return lambda r, d: make_raw_image(r, build)

    def test_the_raw_builder_itself_makes_an_accepted_image(self):
        code, out, err, _ = self.check(self.raw(lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s]}' % (OCI_MANIFEST, c, l)),
                                       want="linux/amd64")
        self.assertEqual(code, 0, err)

    def test_a_layers_key_that_differs_only_by_case_is_refused(self):
        build = lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "Layers": [%s]}' % (OCI_MANIFEST, c, l, EVIL_LAYER)
        err = self.refused(self.raw(build), "Layers beside layers", want="linux/amd64")
        self.assertIn("allowlist", err)

    def test_other_spellings_of_allowed_keys_are_refused(self):
        spellings = [
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "Config": %s, "config": %s, "layers": [%s]}' % (OCI_MANIFEST, c, c, l),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "LAYERS": []}' % (OCI_MANIFEST, c, l),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "layers\u017f": []}' % (OCI_MANIFEST, c, l),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "\u212aey": 1}' % (OCI_MANIFEST, c, l),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "subject": {}}' % (OCI_MANIFEST, c, l),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s]}' % (OCI_MANIFEST, c, l.replace('"digest"', '"Digest"')),
            lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s]}' % (OCI_MANIFEST, c, l[:-1] + ', "data": "eA=="}'),
        ]
        for i, b in enumerate(spellings):
            self.refused(self.raw(b), "spelling %d" % i, want="linux/amd64")

    def test_a_duplicate_key_is_refused_whatever_the_value(self):
        dups = {
            "scalar": lambda c, l: '{"schemaVersion": 2, "schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s]}' % (OCI_MANIFEST, c, l),
            "array with the same leaves": lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "layers": [%s]}' % (OCI_MANIFEST, c, l, EVIL_LAYER),
            "object with other children": lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "config": {"annotations": {"a": "b"}}, "layers": [%s]}' % (OCI_MANIFEST, c, l),
            "scalar then object": lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "annotations": "x", "annotations": {"a": "b"}}' % (OCI_MANIFEST, c, l),
            "empty then object": lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "annotations": {}, "annotations": {"a": "b"}}' % (OCI_MANIFEST, c, l),
            "escaped spelling of the same key": lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "l\\u0061yers": []}' % (OCI_MANIFEST, c, l),
        }
        for name, b in dups.items():
            err = self.refused(self.raw(b), name, want="linux/amd64")
        # and the well-formed neighbour of each (same keys, no repeat) is accepted, so the check is not refusing everything
        ok = lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "annotations": {"a": "b"}}' % (OCI_MANIFEST, c, l)
        code, out, err, _ = self.check(self.raw(ok), want="linux/amd64")
        self.assertEqual(code, 0, err)

    def test_a_duplicate_key_in_the_index_or_the_layout_marker_is_refused(self):
        def index_dup(r, d):
            digest = make_image(r)
            text = open(os.path.join(r, "index.json")).read()
            open(os.path.join(r, "index.json"), "w").write(text[:-1] + ', "schemaVersion": 2}')
            return digest
        self.refused(index_dup, "index dup")

        def marker_dup(r, d):
            digest = make_image(r)
            open(os.path.join(r, "oci-layout"), "w").write('{"imageLayoutVersion": "1.0.0", "imageLayoutVersion": "1.0.0"}')
            return digest
        self.refused(marker_dup, "marker dup")

        def index_case(r, d):
            digest = make_image(r)
            rewrite_json(os.path.join(r, "index.json"), lambda o: o["manifests"][0].update(Platform={"os": "linux"}))
            return digest
        self.refused(index_case, "Platform")

    def test_a_config_with_a_second_spelling_of_os_or_a_duplicate_is_refused(self):
        def cfg_case(r, d):
            cfg = jb({"architecture": "amd64", "os": "linux", "OS": "windows", "rootfs": {"type": "layers", "diff_ids": []}})
            return self.config_image(r, cfg)

        def cfg_dup(r, d):
            return self.config_image(r, b'{"architecture": "amd64", "os": "linux", "os": "windows", "rootfs": {"type": "layers", "diff_ids": []}}')
        self.refused(cfg_case, "OS", want="linux/amd64")
        self.refused(cfg_dup, "dup os", want="linux/amd64")

    def config_image(self, r, cfg):
        put(r, cfg)
        layer = b"layer-0-amd64" + b"\0" * 64
        put(r, layer)
        m = jb({"schemaVersion": 2, "mediaType": OCI_MANIFEST, "config": desc(OCI_CONFIG, cfg), "layers": [desc(LAYER_GZ, layer)]})
        put(r, m)
        write_top(r, desc(OCI_MANIFEST, m))
        return "x"

    # ---- names and digests are matched whole, not line by line
    def test_a_blob_name_with_a_newline_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            open(os.path.join(r, "blobs", "sha256", "a" * 64 + "\nX"), "w").write("x")
            return digest
        self.refused(build, "name\\nX")

    def test_a_digest_with_a_newline_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            rewrite_json(os.path.join(r, "index.json"), lambda o: o["manifests"][0].update(digest=o["manifests"][0]["digest"] + "\nX"))
            return digest
        self.refused(build, "digest\\nX")

        def layer_digest(r, d):
            digest = make_image(r, ("linux/amd64",))
            top = json.load(open(os.path.join(r, "index.json")))["manifests"][0]
            m = json.load(open(blob_path(r, top["digest"])))
            m["layers"][0]["digest"] += "\nX"
            data = jb(m)
            put(r, data)
            write_top(r, desc(OCI_MANIFEST, data))
            return digest
        self.refused(layer_digest, "layer digest\\nX", want="linux/amd64")

    # ---- the path to the layout
    def test_a_symlink_in_the_path_to_the_layout_is_refused_but_only_below_the_root(self):
        with tempfile.TemporaryDirectory() as d:
            real = os.path.join(d, "real")
            os.makedirs(os.path.join(real, "image"))
            make_image(os.path.join(real, "image"))
            ws = os.path.join(d, "ws")
            os.makedirs(ws)
            os.symlink(real, os.path.join(ws, ".glidepath-out"))
            script = 'source "%s"; oci_layout_validate "%s" image "linux/amd64,linux/arm64" "%s"' % (LIB, os.path.join(ws, ".glidepath-out", "image"), ws)
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
            self.assertNotEqual(p.returncode, 0)
            self.assertIn("symlink", p.stderr)
            # the same layout reached without a symlink in the path passes
            os.remove(os.path.join(ws, ".glidepath-out"))
            os.rename(real, os.path.join(ws, ".glidepath-out"))
            script = script.replace(os.path.join(ws, ".glidepath-out", "image"), os.path.join(ws, ".glidepath-out", "image"))
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
            self.assertEqual(p.returncode, 0, p.stderr)

    # ---- a real mode=max cache is big
    def test_a_cache_with_far_more_than_512_layers_is_accepted(self):
        code, out, err, digest = self.check(lambda r, d: make_cache(r, layers=700), kind="cache", want="")
        self.assertEqual(code, 0, err)
        # while an image is still held to the image cap
        self.refused(lambda r, d: make_image(r, ("linux/amd64",), layers=5), "image layer count", want="linux/amd64", env={"OCI_MAX_LAYERS": "4"})

    # ---- platforms
    def test_an_extra_platform_is_refused(self):
        err = self.refused(lambda r, d: make_image(r, ("linux/amd64", "linux/arm64", "linux/riscv64")), "extra platform")
        self.assertIn("riscv64", err)

    def test_a_missing_platform_is_refused(self):
        self.refused(lambda r, d: make_image(r, ("linux/amd64",), force_index=True), "missing platform")

    def test_an_attestation_style_unknown_platform_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            top = json.load(open(os.path.join(r, "index.json")))["manifests"][0]
            idx = json.load(open(blob_path(r, top["digest"])))
            m = image_manifest(r, "amd64")
            idx["manifests"].append(desc(OCI_MANIFEST, m, platform={"os": "unknown", "architecture": "unknown"}))
            data = jb(idx)
            put(r, data)
            write_top(r, desc(OCI_INDEX, data))
            return digest
        self.refused(build, "unknown/unknown")

    def test_a_duplicate_platform_is_refused(self):
        self.refused(lambda r, d: make_image(r, ("linux/amd64", "linux/amd64")), "duplicate", want="linux/amd64,linux/arm64")

    def test_a_config_that_disagrees_with_its_platform_is_refused(self):
        self.refused(lambda r, d: make_image(r, cfg_arch="s390x"), "config arch")

    def test_a_non_linux_platform_is_refused(self):
        self.refused(lambda r, d: make_image(r, ("windows/amd64", "linux/arm64")), "windows")

    def test_a_nested_index_is_refused(self):
        def build(r, d):
            digest = make_image(r)
            top = json.load(open(os.path.join(r, "index.json")))["manifests"][0]
            inner = jb({"schemaVersion": 2, "mediaType": OCI_INDEX, "manifests": [dict(top, platform={"os": "linux", "architecture": "amd64"})]})
            put(r, inner)
            outer = jb({"schemaVersion": 2, "mediaType": OCI_INDEX,
                        "manifests": [desc(OCI_INDEX, inner, platform={"os": "linux", "architecture": "amd64"}),
                                      dict(top, platform={"os": "linux", "architecture": "arm64"})]})
            put(r, outer)
            write_top(r, desc(OCI_INDEX, outer))
            return digest
        self.refused(build, "nested index")


# ------------------------------------------------------------------ the push step, end to end with a fake oras
class PushStep(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.step = step("push-image", "push")
        cls.script = cls.step["script"]

    def run_push(self, setup, cache=None, pr=False, repo=None, version="1.2.3", config=None, resolve=None, extra_env=None, mutate=None):
        """-> (exit code, oras calls (list of argv strings), results dict, stderr)"""
        with tempfile.TemporaryDirectory() as d:
            ws, bin_, res, creds, lib = (os.path.join(d, x) for x in ("ws", "bin", "res", "creds", "lib"))
            for p in (ws, bin_, res, creds, lib):
                os.makedirs(p)
            open(os.path.join(creds, "config.json"), "w").write('{"auths": {}}')
            open(os.path.join(lib, "otel.sh"), "w").write("otel_task_span_send() { :; }\n")
            log = os.path.join(d, "oras.log")
            out = os.path.join(ws, ".glidepath-out")
            os.makedirs(out)
            image_dir = os.path.join(out, "image")
            os.makedirs(image_dir)
            digest = setup(image_dir, d)
            if cache:
                cdir = os.path.join(out, "cache")
                os.makedirs(cdir)
                cache(cdir, d)
            if mutate:
                mutate(ws, d)
            state = os.path.join(d, "pushed")
            # resolve answers FAKE_RESOLVE once something was pushed; before that FAKE_RESOLVE_PRE, or "not found"
            executable(os.path.join(bin_, "oras"),
                       'echo "$@" >> "%s"\ncase "$1" in\n cp) touch "%s";;\n resolve) if [ -e "%s" ]; then echo "${FAKE_RESOLVE:-}"; '
                       'elif [ -n "${FAKE_RESOLVE_PRE:-}" ]; then echo "${FAKE_RESOLVE_PRE}"; else echo "not found" >&2; exit 1; fi;;\nesac' % (log, state, state))
            script = self.script.replace("$(workspaces.source.path)", ws)
            paths = {}
            for name in re.findall(r"\$\(results\.([A-Za-z_-]+)\.path\)", script):
                paths[name] = os.path.join(res, name)
                script = script.replace("$(results.%s.path)" % name, paths[name])
            repo = repo or ("registry.example/o/app-pr" if pr else "registry.example/o/app")
            env = dict(os.environ, PATH=bin_ + ":" + os.environ["PATH"], PLATFORM_LIB=lib, DOCKER_CONFIG=creds,
                       IMAGE_REPO=repo, GIT_REVISION=SHA, PR_TAG_ONLY="true" if pr else "false", VERSION=version,
                       CONFIG_JSON=json.dumps(config if config is not None else {"build": {}}), FLOW_TRACEPARENT="", STAGE_SPAN_ID="",
                       FAKE_RESOLVE=digest if resolve is None else resolve, MAX_IMAGE_BYTES="1000000", MAX_CACHE_BYTES="1000000")
            env.update(extra_env or {})
            p = subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=env, cwd=d)
            calls = open(log).read().splitlines() if os.path.exists(log) else []
            results = {n: open(path).read() for n, path in paths.items() if os.path.exists(path)}
            return p.returncode, calls, results, p.stderr, digest

    def test_a_main_build_pushes_the_image_and_the_main_cache_and_reports_what_it_pushed(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r))
        self.assertEqual(code, 0, err)
        cps = [c for c in calls if c.startswith("cp ")]
        self.assertEqual(len(cps), 2, calls)
        image_cp = [c for c in cps if "buildcache" not in c][0]
        self.assertIn("--from-oci-layout", image_cp)
        self.assertTrue(image_cp.endswith(" registry.example/o/app:1.2.3-" + SHA[:7]), image_cp)
        self.assertIn("@" + digest, image_cp)
        cache_cp = [c for c in cps if "buildcache" in c][0]
        self.assertTrue(cache_cp.endswith(" registry.example/o/app-buildcache:latest"), cache_cp)
        # the digest is the one it computed and the registry confirmed
        self.assertEqual(results["image-digest"], digest)
        self.assertEqual(results["IMAGE_DIGEST"], digest)
        self.assertEqual(results["image-ref"], "registry.example/o/app:1.2.3-" + SHA[:7])
        self.assertEqual(results["image-repo"], "registry.example/o/app")
        self.assertEqual(results["IMAGE_URL"], "registry.example/o/app")
        self.assertEqual(results["version"], "1.2.3")
        self.assertTrue(any(c.startswith("resolve ") for c in calls))
        # the image is pushed first: a refused cache never blocks nor precedes it
        self.assertLess(calls.index(image_cp), calls.index(cache_cp))

    def test_a_pull_request_build_never_writes_a_cache_even_if_its_workspace_holds_one(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r), pr=True)
        self.assertEqual(code, 0, err)
        self.assertEqual([c for c in calls if "buildcache" in c], [], "a PR build must not write any cache ref")
        cps = [c for c in calls if c.startswith("cp ")]
        self.assertEqual(len(cps), 1, calls)
        self.assertTrue(cps[0].endswith(" registry.example/o/app-pr:" + SHA[:12]), cps[0])
        self.assertEqual(results["image-ref"], "registry.example/o/app-pr:" + SHA[:12])

    def test_a_pull_request_build_cannot_push_to_the_main_repository_or_its_cache(self):
        # whatever the file said: PR_TAG_ONLY=true with a repository that is not the -pr one is refused before anything is pushed
        code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r),
                                                    repo="registry.example/o/app", extra_env={"PR_TAG_ONLY": "true"})
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])
        self.assertEqual(results, {})

    def test_a_hostile_layout_is_refused_and_nothing_is_pushed(self):
        def symlinked(r, d):
            digest = make_image(r)
            layer = os.listdir(os.path.join(r, "blobs", "sha256"))[0]
            os.remove(os.path.join(r, "blobs", "sha256", layer))
            os.symlink("/etc/passwd", os.path.join(r, "blobs", "sha256", layer))
            return digest
        for name, setup in (("symlink", symlinked),
                            ("extra platform", lambda r, d: make_image(r, ("linux/amd64", "linux/arm64", "linux/riscv64"))),
                            ("media type", lambda r, d: make_image(r, layer_media="text/html"))):
            code, calls, results, err, _ = self.run_push(setup)
            self.assertNotEqual(code, 0, name)
            self.assertEqual([c for c in calls if c.startswith("cp ")], [], name)
            self.assertEqual(results, {}, name)

    def test_oversized_output_is_refused(self):
        code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), extra_env={"MAX_IMAGE_BYTES": "10"})
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])

    def test_the_platforms_come_from_the_validated_config_not_from_the_layout(self):
        code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), config={"build": {"platforms": ["amd64"]}})
        self.assertNotEqual(code, 0, "an arm64 image the cicd.yaml did not ask for must be refused")
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])
        code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r, ("linux/amd64",)), config={"build": {"platforms": ["amd64"]}})
        self.assertEqual(code, 0, err)

    def test_the_digest_must_be_confirmed_by_the_registry(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), resolve="sha256:" + "0" * 64)
        self.assertNotEqual(code, 0)
        self.assertNotIn("image-digest", results)
        self.assertNotIn("IMAGE_DIGEST", results)

    def test_the_digest_is_not_read_from_anything_the_build_wrote(self):
        def setup(r, d):
            digest = make_image(r)
            for name in ("digest", "image-digest", "build-metadata.json", "metadata.json"):
                open(os.path.join(os.path.dirname(r), name), "w").write('{"containerimage.digest": "sha256:%s"}' % ("e" * 64))
            return digest
        code, calls, results, err, digest = self.run_push(setup)
        self.assertEqual(code, 0, err)
        self.assertEqual(results["image-digest"], digest)
        self.assertNotIn("e" * 64, "".join(results.values()))

    def test_a_version_that_is_not_a_tag_is_refused(self):
        for bad in ("1.0;rm -rf /", "a b", "../x", "", "-1", "1.0\n2.0", "$(id)", "x" * 130):
            code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), version=bad)
            self.assertNotEqual(code, 0, repr(bad))
            self.assertEqual([c for c in calls if c.startswith("cp ")], [], repr(bad))

    def test_a_repository_that_is_not_a_repository_is_refused(self):
        for bad in ("registry.example/o/app;id", "registry.example/o/../x", "REGISTRY/o/App", "registry.example", "registry.example/o/app:tag",
                    "-evil/o/app"):
            code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), repo=bad)
            self.assertNotEqual(code, 0, bad)
            self.assertEqual([c for c in calls if c.startswith("cp ")], [], bad)

    def test_a_missing_layout_fails_without_pushing(self):
        def setup(r, d):
            os.rmdir(r)
            return "sha256:" + "0" * 64
        code, calls, results, err, _ = self.run_push(setup)
        self.assertNotEqual(code, 0)
        self.assertEqual(calls, [])

    def test_an_invalid_cache_is_skipped_with_a_warning_and_never_fails_the_task_or_blocks_the_image(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r, layer_media="text/html"))
        self.assertEqual(code, 0, err)
        self.assertEqual([c for c in calls if "buildcache" in c], [], "an invalid cache is never pushed")
        self.assertEqual(len([c for c in calls if c.startswith("cp ")]), 1, "the image is pushed")
        self.assertEqual(results["image-digest"], digest)
        self.assertIn("warning", err)
        self.assertIn("refusing", err)
        self.assertTrue(results["cache"].startswith("skipped"), results["cache"])

    def test_an_application_whose_own_repository_ends_in_pr_publishes_its_cache(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r), repo="registry.example/o/sample-pr")
        self.assertEqual(code, 0, err)
        self.assertEqual(results["cache"], "published")
        self.assertTrue([c for c in calls if c.endswith(" registry.example/o/sample-pr-buildcache:latest")], calls)

    def test_the_cache_result_says_what_happened(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r))
        self.assertEqual(results["cache"], "published")
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), pr=True)
        self.assertTrue(results["cache"].startswith("skipped"), results["cache"])
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r))
        self.assertTrue(results["cache"].startswith("skipped"), results["cache"])

    def test_a_cache_with_hundreds_of_layers_is_published(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), cache=lambda r, d: make_cache(r, layers=600))
        self.assertEqual(code, 0, err)
        self.assertEqual(results["cache"], "published")

    def test_the_pinned_reference_is_the_digest_of_what_was_pushed(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r))
        self.assertEqual(results["image-pinned-ref"], "registry.example/o/app@" + digest)
        self.assertEqual(results["image-ref"], "registry.example/o/app:1.2.3-" + SHA[:7])

    def test_an_existing_main_tag_with_another_digest_is_refused(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), extra_env={"FAKE_RESOLVE_PRE": "sha256:" + "9" * 64})
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])
        self.assertEqual(results, {})
        self.assertIn("already exists", err)

    def test_an_existing_main_tag_with_the_same_digest_is_a_harmless_rerun(self):
        def setup(r, d):
            return make_image(r)
        code, calls, results, err, digest = self.run_push(setup, resolve=None, extra_env={})
        # FAKE_RESOLVE_PRE must equal the layout's digest, known only after setup: run once to learn it
        code2, calls2, results2, err2, _ = self.run_push(setup, extra_env={"FAKE_RESOLVE_PRE": digest})
        self.assertEqual(code2, 0, err2)
        self.assertEqual(results2["image-digest"], digest)

    def test_a_pull_request_tag_may_be_replaced(self):
        # PR images are rebuilt per push to the PR (not reproducible), so their tag is not held to the check
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r), pr=True, extra_env={"FAKE_RESOLVE_PRE": "sha256:" + "9" * 64})
        self.assertEqual(code, 0, err)

    def test_a_symlinked_output_directory_is_refused(self):
        def mutate(ws, d):
            real = os.path.join(d, "elsewhere")
            os.rename(os.path.join(ws, ".glidepath-out"), real)
            os.symlink(real, os.path.join(ws, ".glidepath-out"))
        code, calls, results, err, _ = self.run_push(lambda r, d: make_image(r), mutate=mutate)
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])
        self.assertIn("symlink", err)

    def test_the_layers_case_is_refused_through_the_real_step(self):
        build = lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "Layers": [%s]}' % (OCI_MANIFEST, c, l, EVIL_LAYER)
        code, calls, results, err, _ = self.run_push(lambda r, d: make_raw_image(r, build), config={"build": {"platforms": ["amd64"]}})
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])
        self.assertEqual(results, {})

    def test_a_duplicate_key_is_refused_through_the_real_step(self):
        build = lambda c, l: '{"schemaVersion": 2, "mediaType": "%s", "config": %s, "layers": [%s], "layers": [%s]}' % (OCI_MANIFEST, c, l, EVIL_LAYER)
        code, calls, results, err, _ = self.run_push(lambda r, d: make_raw_image(r, build), config={"build": {"platforms": ["amd64"]}})
        self.assertNotEqual(code, 0)
        self.assertEqual([c for c in calls if c.startswith("cp ")], [])

    def test_no_cache_output_is_a_warning_not_a_failure(self):
        code, calls, results, err, digest = self.run_push(lambda r, d: make_image(r))
        self.assertEqual(code, 0, err)
        self.assertEqual([c for c in calls if "buildcache" in c], [])


# ------------------------------------------------------------------ the build step
class BuildStep(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.step = step("build-image", "build")

    def run_build(self, pr, repo=None, platforms="linux/arm64,linux/amd64"):
        """-> (exit code, buildctl argv as one string, workspace listing)"""
        with tempfile.TemporaryDirectory() as d:
            ws, bin_ = os.path.join(d, "ws"), os.path.join(d, "bin")
            os.makedirs(ws)
            os.makedirs(bin_)
            os.makedirs(os.path.join(ws, ".glidepath-out", "stale"))        # a pre-committed leftover must be removed
            os.symlink(d, os.path.join(ws, "link"))
            log = os.path.join(d, "buildctl.log")
            executable(os.path.join(bin_, "buildctl-daemonless.sh"), 'echo "$@" > "%s"' % log)
            script = self.step["script"].replace("$(workspaces.source.path)", ws)
            env = dict(os.environ, PATH=bin_ + ":" + os.environ["PATH"], CONTAINERFILE_PATH="./Containerfile", PLATFORMS=platforms,
                       IMAGE_REPO=repo or ("registry.example/o/app-pr" if pr else "registry.example/o/app"),
                       PR_TAG_ONLY="true" if pr else "false", OCI_VERSION="1.2.3", OCI_REVISION=SHA, OCI_SOURCE="https://example/o/app",
                       OCI_CREATED="2026-11-14T00:00:00Z")
            p = subprocess.run(["sh", "-c", script], capture_output=True, text=True, env=env, cwd=d)
            argv = open(log).read() if os.path.exists(log) else ""
            return p.returncode, argv, os.listdir(os.path.join(ws, ".glidepath-out")) if os.path.isdir(os.path.join(ws, ".glidepath-out")) else None

    def test_a_main_build_exports_its_cache_to_the_workspace_never_to_the_registry(self):
        code, argv, listing = self.run_build(pr=False)
        self.assertEqual(code, 0)
        self.assertIn("--export-cache type=local,dest=", argv)
        self.assertNotRegex(argv, r"--export-cache type=registry")
        self.assertNotIn("push=true", argv)
        self.assertNotIn("type=image", argv)
        self.assertRegex(argv, r"--output type=oci,dest=\S+/\.glidepath-out/image,tar=false")
        self.assertEqual(listing, [], "leftovers in the output directory must be removed before the build")

    def test_a_pull_request_build_imports_the_main_cache_and_exports_nothing(self):
        code, argv, _ = self.run_build(pr=True)
        self.assertEqual(code, 0)
        self.assertNotIn("--export-cache", argv)
        self.assertIn("--import-cache type=registry,ref=registry.example/o/app-buildcache:latest", argv)
        self.assertNotIn("app-pr-buildcache", argv)

    def test_an_application_whose_own_repository_ends_in_pr_keeps_its_own_cache(self):
        code, argv, _ = self.run_build(pr=False, repo="registry.example/o/sample-pr")
        self.assertIn("--import-cache type=registry,ref=registry.example/o/sample-pr-buildcache:latest", argv)
        self.assertNotIn("ref=registry.example/o/sample-buildcache", argv)
        self.assertIn("--export-cache type=local", argv)
        # its pull request build (repository sample-pr-pr) still reads the main cache, which is sample-pr's
        code, argv, _ = self.run_build(pr=True, repo="registry.example/o/sample-pr-pr")
        self.assertIn("--import-cache type=registry,ref=registry.example/o/sample-pr-buildcache:latest", argv)
        self.assertNotIn("--export-cache", argv)

    def test_the_main_build_imports_the_main_cache(self):
        code, argv, _ = self.run_build(pr=False)
        self.assertIn("--import-cache type=registry,ref=registry.example/o/app-buildcache:latest", argv)

    def test_every_requested_platform_is_built_in_the_one_invocation(self):
        code, argv, _ = self.run_build(pr=False, platforms="linux/amd64")
        self.assertIn("--opt platform=linux/amd64", argv)
        code, argv, _ = self.run_build(pr=False)
        self.assertIn("--opt platform=linux/arm64,linux/amd64", argv)


# ------------------------------------------------------------------ rendered accounts, volumes, results
def volumes(task):
    return TASKS[task]["spec"].get("volumes", [])


def all_mounts(task):
    return [m for s in TASKS[task]["spec"]["steps"] for m in s.get("volumeMounts", [])]


class RenderedTasks(unittest.TestCase):
    def test_the_build_pod_holds_no_registry_push_credential_and_no_token(self):
        text = yaml.safe_dump(TASKS["build-image"]["spec"])
        self.assertNotIn("registry-credentials", text)
        self.assertNotIn("serviceAccountToken", text)
        self.assertNotIn("projected", text)
        self.assertNotIn("kubectl", text)
        secrets = [v for v in volumes("build-image") if "secret" in v]
        # the only secret volume is the optional read-only pull credential, by its own name
        self.assertEqual([v["secret"]["secretName"] for v in secrets], ["build-pull-credentials"])
        self.assertIs(secrets[0]["secret"]["optional"], True)
        self.assertEqual([v for v in volumes("build-image") if "secret" not in v], [])
        for m in all_mounts("build-image"):
            self.assertIs(m.get("readOnly"), True, m)

    def test_the_pull_credential_name_is_a_value(self):
        docs = render("--set", "buildPullCredential.secretName=my-pull")
        task = [d for d in docs if d["kind"] == "Task" and d["metadata"]["name"] == "build-image"][0]
        self.assertEqual([v["secret"]["secretName"] for v in task["spec"]["volumes"] if "secret" in v], ["my-pull"])

    def test_the_pull_credential_is_never_the_push_credential(self):
        for bad in ("registry-credentials",):
            r = subprocess.run(["helm", "template", "c", CATALOG, "-f", os.path.join(CATALOG, "tests", "values.example.yaml"),
                                "--set", "buildPullCredential.secretName=" + bad], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0, "the build pod must not be able to mount the push credential by configuration")

    def test_the_build_task_has_no_image_results(self):
        names = {r["name"] for r in TASKS["build-image"]["spec"]["results"]}
        for forbidden in ("image-digest", "IMAGE_DIGEST", "IMAGE_URL", "image-ref", "image-repo"):
            self.assertNotIn(forbidden, names)
        self.assertEqual(names, {"version"})

    def test_the_build_step_runs_the_builder_image_and_only_that_one(self):
        images = {s["name"]: s["image"] for s in TASKS["build-image"]["spec"]["steps"]}
        self.assertTrue(images["build"].startswith("moby/buildkit:"), images)
        self.assertIn("@sha256:", images["build"])

    def test_the_push_pod_mounts_the_push_credential_and_nothing_executes_workspace_content(self):
        task = TASKS["push-image"]["spec"]
        self.assertEqual([v["secret"]["secretName"] for v in task["volumes"] if "secret" in v], ["registry-credentials"])
        ws = [w for w in task["workspaces"] if w["name"] == "source"][0]
        self.assertIs(ws.get("readOnly"), True, "the push pod only reads the workspace")
        for s in task["steps"]:
            self.assertNotIn("$(params.agent-image)", s["image"])
            self.assertTrue(s["image"].startswith(rendered_toolbox()), s["image"])
        script = "\n".join(s.get("script", "") for s in task["steps"])
        # nothing sourced from, evaluated from, or run out of the workspace; no git, no helm, no build tools
        self.assertNotRegex(script, r'(^|\n)\s*(source|\.)\s+"?\$\{?(WS|OUT|SRC|IMAGE_DIR|CACHE_DIR|\$\(workspaces)')
        self.assertNotRegex(script, r"\beval\b")
        code = "\n".join(l for l in script.splitlines() if not l.strip().startswith("#"))
        for tool in ("git ", "helm ", "buildctl", "docker ", "make ", "npm ", "python"):
            self.assertNotIn(tool, code)

    def test_the_push_task_reports_the_trusted_results(self):
        names = {r["name"] for r in TASKS["push-image"]["spec"]["results"]}
        self.assertEqual(names, {"image-digest", "image-ref", "image-pinned-ref", "image-repo", "version", "IMAGE_URL", "IMAGE_DIGEST", "cache"})

    def test_the_image_limits_are_values(self):
        docs = render("--set", "imagePush.maxImageBytes=123", "--set", "imagePush.maxCacheBytes=456")
        task = [d for d in docs if d["kind"] == "Task" and d["metadata"]["name"] == "push-image"][0]
        env = {e["name"]: e["value"] for s in task["spec"]["steps"] for e in s.get("env", [])}
        self.assertEqual(env["MAX_IMAGE_BYTES"], "123")
        self.assertEqual(env["MAX_CACHE_BYTES"], "456")

    def test_only_the_push_task_and_the_known_credentialed_tasks_mount_the_push_credential(self):
        holders = sorted(n for n, t in TASKS.items() if "registry-credentials" in yaml.safe_dump(t["spec"].get("volumes", [])))
        self.assertIn("push-image", holders)
        self.assertNotIn("build-image", holders)
        for repo_code_task in ("build-source", "run-tests", "run-integration-script"):
            self.assertNotIn(repo_code_task, holders)


def rendered_toolbox():
    return yaml.safe_load(open(os.path.join(CATALOG, "values.yaml")))["toolboxImage"]


# ------------------------------------------------------------------ the pipeline: who takes what from whom
class BuildPipeline(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tasks = {t["name"]: t for t in PIPELINES["build"]["spec"]["tasks"]}

    def test_push_image_follows_build_image_and_takes_its_parameters_from_the_platform(self):
        t = self.tasks["push-image"]
        self.assertEqual(t["runAfter"], ["build-image"])
        params = {p["name"]: p["value"] for p in t["params"]}
        self.assertEqual(params["image-repo"], "$(params.image-repo)")
        self.assertEqual(params["git-revision"], "$(params.git-revision)")
        self.assertEqual(params["pr-tag-only"], "$(params.pr-tag-only)")
        self.assertEqual(params["config-json"], "$(tasks.preflight.results.config-json)")
        self.assertEqual({w["name"] for w in t["workspaces"]}, {"source"})

    def test_everything_downstream_takes_the_image_from_the_push_task(self):
        for name in ("image-scan", "generate-sbom", "attest-api-spec", "build-python-wheel-artifact", "build-go-binary-artifact"):
            self.assertEqual(self.tasks[name]["runAfter"], ["push-image"], name)
        for name in ("image-scan", "generate-sbom", "attest-api-spec"):
            image_ref = [p["value"] for p in self.tasks[name]["params"] if p["name"] == "image-ref"][0]
            self.assertEqual(image_ref, "$(tasks.push-image.results.image-pinned-ref)", name)
        for name in ("build-python-wheel-artifact", "build-go-binary-artifact"):
            version = [p["value"] for p in self.tasks[name]["params"] if p["name"] == "version"][0]
            self.assertEqual(version, "$(tasks.push-image.results.version)", name)
        results = {r["name"]: r["value"] for r in PIPELINES["build"]["spec"]["results"]}
        self.assertEqual(results["IMAGE_DIGEST"], "$(tasks.push-image.results.image-digest)")
        self.assertEqual(results["IMAGE_URL"], "$(tasks.push-image.results.image-repo)")
        debrief = PIPELINES["build"]["spec"]["finally"][0]
        self.assertEqual([p["value"] for p in debrief["params"] if p["name"] == "image-ref-from-task"], ["push-image"])

    def test_no_task_after_the_push_consumes_a_tag_only_reference(self):
        # a tag can be re-pointed after the push: everything that scans, describes, attests or publishes takes repo@digest
        for t in PIPELINES["build"]["spec"]["tasks"]:
            for p in t.get("params", []):
                self.assertNotEqual(p["value"], "$(tasks.push-image.results.image-ref)", "%s param %s consumes the tag" % (t["name"], p["name"]))
        for name in ("image-scan", "generate-sbom", "attest-api-spec"):
            self.assertIn(name, self.tasks)
        # the tag form is for display and for chaining only: the finally task looks it up, by name, after the run
        debrief = PIPELINES["build"]["spec"]["finally"][0]
        self.assertEqual([p["value"] for p in debrief["params"] if p["name"] == "image-ref-from-task"], ["push-image"])

    def test_no_result_of_the_build_task_is_used_except_the_version_that_push_image_validates(self):
        texts = {}
        for path, _, files in os.walk(os.path.join(CATALOG, "templates")):
            for f in files:
                p = os.path.join(path, f)
                texts[p] = open(p).read()
        uses = []
        for p, t in texts.items():
            code = "\n".join(l for l in t.splitlines() if not l.strip().startswith("#"))
            uses += [(os.path.basename(p), m) for m in re.findall(r"\$\(tasks\.build-image\.results\.[A-Za-z_-]+\)", code)]
        self.assertEqual(uses, [("build.yaml", "$(tasks.build-image.results.version)")])
        # and no live lookup of the build task's TaskRun results either: the notifications read the push task's
        for p, t in texts.items():
            self.assertNotIn("pipelineTask=build-image", t, p)

    def test_the_provenance_check_accepts_the_push_task_as_the_build_marker(self):
        text = open(os.path.join(CATALOG, "templates", "tasks", "verify-image-provenance.yaml")).read()
        self.assertIn('.name == "push-image"', text)


if __name__ == "__main__":
    unittest.main()
