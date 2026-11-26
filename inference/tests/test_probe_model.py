"""The explicit load-only model probe."""
from __future__ import annotations

import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from inference import adapters, dispatch
from inference.adapters.base import ModelUnavailable

from .fixtures import entry, make_fixture_dir, patched_catalog, patched_dependencies, run_dispatch, parse_single_frame


class _Stdout:
    """Minimal sys.stdout stand-in exposing a writable .buffer for main()."""

    def __init__(self, buffer: io.BytesIO) -> None:
        self.buffer = buffer

    def write(self, _text: str) -> int:
        return 0

    def flush(self) -> None:
        return None


class _StubAdapter:
    def __init__(self, error: Exception | None = None) -> None:
        self.error = error
        self.loaded = False

    def load(self) -> None:
        self.loaded = True
        if self.error is not None:
            raise self.error


def stub_load(error: Exception | None = None):
    stub = _StubAdapter(error)
    calls = []

    def make_adapter(_entry, _context):
        calls.append(_context)
        return stub

    return stub, calls, mock.patch.object(adapters, "make_adapter", make_adapter)


class ProbeModelTest(unittest.TestCase):
    def test_available_identity_after_successful_load(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, calls, patch_adapter = stub_load()
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/model", "rev-1")
        self.assertTrue(stub.loaded)
        self.assertEqual(len(calls), 1)
        self.assertTrue(row["available"])
        self.assertNotIn("unavailable_code", row)
        self.assertEqual(row["model_id"], "synthetic/model")
        self.assertEqual(row["model_revision"], "rev-1")
        self.assertEqual(row["operation"], "visual_embedding")
        self.assertEqual(row["dimension"], 768)
        # The probe context must never be wired to protocol output or a source.
        self.assertEqual(calls[0].device, "cpu")
        self.assertEqual(calls[0].dtype, "float32")

    def test_failed_load_is_model_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load(ModelUnavailable("locked model could not be loaded locally"))
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/model", "rev-1")
        self.assertTrue(stub.loaded)
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_oom_load_is_resource_exhausted(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load(MemoryError("CUDA out of memory"))
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/model", "rev-1")
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "resource_exhausted")

    def test_unexpected_load_failure_is_model_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load(RuntimeError("boom"))
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/model", "rev-1")
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_wrong_revision_cannot_satisfy_the_probe(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load()
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/model", "rev-9")
        self.assertFalse(stub.loaded, "an unlocked identity must never reach the loader")
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")
        self.assertEqual(row["model_id"], "synthetic/model")
        self.assertEqual(row["model_revision"], "rev-9")

    def test_unknown_model_identity_has_no_operation(self):
        with tempfile.TemporaryDirectory() as tmp:
            stub, _calls, patch_adapter = stub_load()
            with patched_catalog(Path(tmp), [entry()]), patch_adapter:
                row = adapters.probe_model(Path(tmp) / "models", "cpu", "float32",
                                           "nobody/unknown", "rev-x")
        self.assertFalse(row["available"])
        self.assertEqual(row["operation"], "")
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_gated_identity_is_blocked_before_the_loader(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="gated", model_id="synthetic/gated", family="siglip2",
                          operations=("visual_embedding",), dimension=768,
                          artifact_status="blocked_gated")
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load()
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/gated", "rev-1")
        self.assertFalse(stub.loaded)
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_incompatible_profile_is_blocked_before_the_loader(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="fp16", model_id="synthetic/fp16", family="siglip2",
                          operations=("visual_embedding",), dimension=768,
                          dtypes=("float16",))
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load()
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter:
                row = adapters.probe_model(root, "cpu", "float32", "synthetic/fp16", "rev-1")
        self.assertFalse(stub.loaded)
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "unsupported_input")


class ProbeModelDispatchTest(unittest.TestCase):
    """The dispatcher's --probe-model mode frames one capability like inventory."""

    def test_main_writes_a_single_capabilities_frame(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            stub, _calls, patch_adapter = stub_load()
            buffer = io.BytesIO()
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True), patch_adapter, \
                    mock.patch.object(sys, "stdout", _Stdout(buffer)):
                code = dispatch.main(["--probe-model", "synthetic/model", "--probe-revision",
                                      "rev-1", "--model-dir", str(root),
                                      "--device", "cpu", "--dtype", "float32"])
        self.assertEqual(code, 0)
        self.assertTrue(stub.loaded)
        lines = [line for line in buffer.getvalue().decode("utf-8").splitlines() if line.strip()]
        self.assertEqual(len(lines), 1, "the probe must emit exactly one protocol frame")
        frame = json.loads(lines[0])
        self.assertEqual(frame["type"], "capabilities")
        self.assertEqual(len(frame["capabilities"]), 1)
        row = frame["capabilities"][0]
        self.assertEqual(row["model_id"], "synthetic/model")
        self.assertEqual(row["model_revision"], "rev-1")
        self.assertTrue(row["available"])
        self.assertNotIn("unavailable_code", row)

    def test_dispatch_probe_model_frame_through_subprocess(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="probed", model_id="synthetic/model", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            # The subprocess cannot be monkey-patched; the real adapter import
            # path fails cleanly as model_unavailable without the heavy
            # dependencies, exactly like the empty-directory inventory.
            proc = run_dispatch("--probe-model", "synthetic/model", "--probe-revision", "rev-1",
                                "--model-dir", str(root), "--device", "cpu", "--dtype", "float32")
        self.assertEqual(proc.returncode, 0, proc.stderr.decode("utf-8", "replace"))
        frame = parse_single_frame(proc)
        self.assertEqual(frame["type"], "capabilities")
        self.assertEqual(len(frame["capabilities"]), 1)
        row = frame["capabilities"][0]
        self.assertEqual(row["model_id"], "synthetic/model")
        self.assertEqual(row["model_revision"], "rev-1")
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")


if __name__ == "__main__":
    unittest.main()
