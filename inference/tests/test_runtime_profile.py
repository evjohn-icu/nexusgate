"""The single shared runtime-profile gate."""
from __future__ import annotations

import importlib
import pathlib
import sys
import tempfile
import types
import unittest
from unittest import mock

from inference import adapters
from inference.adapters.base import ModelUnavailable, UnsupportedInput

from .fixtures import entry, patched_catalog


class _FakeModule(types.ModuleType):
    pass


def installed_module(name: str, **attributes):
    module = _FakeModule(name)
    for key, value in attributes.items():
        setattr(module, key, value)
    return module


class ValidateRuntimeProfileTest(unittest.TestCase):
    def test_rejects_dtype_outside_the_lock(self):
        model = entry(dtypes=("float16",))
        with self.assertRaises(UnsupportedInput):
            adapters.validate_runtime_profile(model, "cpu", "float32")
        adapters.validate_runtime_profile(model, "cpu", "float16")

    def test_rejects_device_outside_the_lock(self):
        model = entry(backends=("cpu",))
        with self.assertRaises(UnsupportedInput):
            adapters.validate_runtime_profile(model, "cuda", "float32")
        adapters.validate_runtime_profile(model, "cpu", "float32")

    def test_default_lock_allows_both_devices_pending_availability(self):
        model = entry()
        adapters.validate_runtime_profile(model, "cpu", "float32")
        with self.assertRaises(ModelUnavailable):
            adapters.validate_runtime_profile(model, "cuda", "float32")

    def test_cuda_requires_a_working_torch(self):
        model = entry(family="siglip2", backend="torch")
        without = installed_module("torch",
                                   cuda=types.SimpleNamespace(is_available=lambda: False))
        with mock.patch.dict(sys.modules, {"torch": without}), mock.patch.dict(
                sys.modules, clear=False):
            importlib.invalidate_caches()
            with self.assertRaises(ModelUnavailable):
                adapters.validate_runtime_profile(model, "cuda", "float32")
        available = installed_module("torch",
                                     cuda=types.SimpleNamespace(is_available=lambda: True))
        with mock.patch.dict(sys.modules, {"torch": available}):
            importlib.invalidate_caches()
            adapters.validate_runtime_profile(model, "cuda", "float32")

    def test_cuda_requires_a_working_tensorflow(self):
        model = entry(family="yamnet", backend="tensorflow")
        empty = installed_module(
            "tensorflow", config=types.SimpleNamespace(list_physical_devices=lambda kind: []))
        with mock.patch.dict(sys.modules, {"tensorflow": empty}):
            importlib.invalidate_caches()
            with self.assertRaises(ModelUnavailable):
                adapters.validate_runtime_profile(model, "cuda", "float32")
        gpu = installed_module(
            "tensorflow",
            config=types.SimpleNamespace(
                list_physical_devices=lambda kind: ["GPU:0"] if kind == "GPU" else []))
        with mock.patch.dict(sys.modules, {"tensorflow": gpu}):
            importlib.invalidate_caches()
            adapters.validate_runtime_profile(model, "cuda", "float32")

    def test_missing_backend_module_is_model_unavailable_on_cuda(self):
        model = entry(backend="torch")
        with mock.patch.object(adapters, "_BACKEND_MODULES",
                               {"torch": "nexusgate_definitely_not_installed"}):
            with self.assertRaises(ModelUnavailable):
                adapters.validate_runtime_profile(model, "cuda", "float32")

    def test_unknown_backend_never_promises_cuda(self):
        model = entry(backend="synthetic")
        with self.assertRaises(ModelUnavailable):
            adapters.validate_runtime_profile(model, "cuda", "float32")

    def test_cpu_requests_never_import_the_backend(self):
        model = entry(backend="torch")
        # An explicit None in sys.modules makes `import torch` fail hard; the
        # CPU path must never reach it.
        with mock.patch.dict(sys.modules, {"torch": None}):
            importlib.invalidate_caches()
            adapters.validate_runtime_profile(model, "cpu", "float32")


class InventoryDimensionRuleTest(unittest.TestCase):
    def test_report_uses_the_shared_dimension_rule(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="nodim", operations=("visual_embedding",), dimension=None)
            root = pathlib.Path(tmp) / "models"
            with patched_catalog(pathlib.Path(tmp), [model]):
                report = adapters.capability_report(root, "cpu", "float32")
        row = report["capabilities"][0]
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")
        self.assertEqual(row["dimension"], 0)

    def test_load_backends_honours_the_lock_and_defaults(self):
        self.assertEqual(adapters.load_backends(entry(backends=("cpu",))), ["cpu"])
        self.assertEqual(adapters.load_backends(entry()), ["cpu", "cuda"])


if __name__ == "__main__":
    unittest.main()
