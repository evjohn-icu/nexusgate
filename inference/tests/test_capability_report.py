"""Capability inventory: typed unavailable codes and the zero-dimension rule."""
from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from inference import adapters, protocol

from .fixtures import entry, make_fixture_dir, patched_catalog, patched_dependencies, run_dispatch, parse_single_frame


class InventoryProbeSubprocessTest(unittest.TestCase):
    """The acceptance probe: real lock, empty model directory, no dependencies."""

    def test_artifact_free_probe_reports_schema_valid_unavailable_entries(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = run_dispatch("--probe", "--model-dir", str(Path(tmp) / "models"),
                                "--device", "cpu", "--dtype", "float32")
        self.assertEqual(proc.returncode, 0, proc.stderr.decode("utf-8", "replace"))
        frame = parse_single_frame(proc)
        self.assertEqual(frame["type"], "capabilities")
        self.assertEqual(frame["schema_version"], "inference.capabilities.v1")
        self.assertGreater(len(frame["capabilities"]), 0)
        for row in frame["capabilities"]:
            self.assertEqual(row["schema_version"], "inference.v1")
            self.assertTrue(row["model_id"], "capability must name a model")
            self.assertTrue(row["model_revision"], "capability must name a revision")
            self.assertTrue(row["backend"])
            self.assertIsInstance(row["dimension"], int)
            self.assertGreaterEqual(row["dimension"], 0)
            self.assertFalse(row["available"])
            self.assertEqual(row["unavailable_code"], "model_unavailable")
        by_operation = {row["operation"]: row for row in frame["capabilities"]}
        self.assertIn("sound_events", by_operation)
        self.assertIn("diarization", by_operation)
        self.assertIn("visual_embedding", by_operation)

    def test_wrong_revision_cannot_satisfy_the_model_probe(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = run_dispatch("--probe-model", "google/siglip2-base-patch16-224",
                                "--probe-revision", "0" * 40,
                                "--model-dir", str(Path(tmp) / "models"),
                                "--device", "cpu", "--dtype", "float32")
        self.assertEqual(proc.returncode, 0, proc.stderr.decode("utf-8", "replace"))
        frame = parse_single_frame(proc)
        self.assertEqual(frame["type"], "capabilities")
        self.assertEqual(len(frame["capabilities"]), 1)
        row = frame["capabilities"][0]
        self.assertEqual(row["model_id"], "google/siglip2-base-patch16-224")
        self.assertEqual(row["model_revision"], "0" * 40)
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_unknown_model_identity_is_never_probeable(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = run_dispatch("--probe-model", "nobody/unknown-model",
                                "--probe-revision", "rev-x",
                                "--model-dir", str(Path(tmp) / "models"),
                                "--device", "cpu", "--dtype", "float32")
        self.assertEqual(proc.returncode, 0)
        row = parse_single_frame(proc)["capabilities"][0]
        self.assertEqual(row["model_id"], "nobody/unknown-model")
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_probe_model_requires_its_revision(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = run_dispatch("--probe-model", "google/siglip2-base-patch16-224",
                                "--model-dir", str(Path(tmp) / "models"))
        self.assertEqual(proc.returncode, 2)
        self.assertEqual(proc.stdout, b"")


class CapabilityReportTest(unittest.TestCase):
    """In-process inventory semantics against a synthetic lock."""

    def test_profile_incompatible_model_is_unavailable_with_unsupported_input(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="fp16-only", model_id="synthetic/fp16", family="siglip2",
                          operations=("visual_embedding",), dimension=768,
                          dtypes=("float16",))
            root = make_fixture_dir(tmp, model)
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float32")
        row = report["capabilities"][0]
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "unsupported_input")

    def test_fp16_model_is_available_on_its_own_profile(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="fp16-only", model_id="synthetic/fp16", family="siglip2",
                          operations=("visual_embedding",), dimension=768,
                          dtypes=("float16",))
            root = make_fixture_dir(tmp, model)
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float16")
        row = report["capabilities"][0]
        self.assertTrue(row["available"])
        self.assertNotIn("unavailable_code", row)
        self.assertEqual(row["dimension"], 768)

    def test_missing_files_report_model_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="absent", model_id="synthetic/absent", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, entry(catalog_id="other", files=()))
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float32")
        row = report["capabilities"][0]
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_missing_dependency_reports_model_unavailable(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="nodeps", model_id="synthetic/nodeps", family="siglip2",
                          operations=("visual_embedding",), dimension=768)
            root = make_fixture_dir(tmp, model)
            with patched_catalog(Path(tmp), [model]), patched_dependencies(False):
                report = adapters.capability_report(root, "cpu", "float32")
        row = report["capabilities"][0]
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_gated_and_pending_entries_stay_blocked(self):
        with tempfile.TemporaryDirectory() as tmp:
            gated = entry(catalog_id="gated", model_id="synthetic/gated", family="siglip2",
                          operations=("visual_embedding",), dimension=768,
                          artifact_status="blocked_gated")
            pending = entry(catalog_id="pending", model_id="synthetic/pending", family="siglip2",
                            operations=("visual_embedding",), dimension=768,
                            artifact_status="pending_download")
            for model in (gated, pending):
                make_fixture_dir(tmp, model)
            root = Path(tmp) / "models"
            with patched_catalog(Path(tmp), [gated, pending]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float32")
        for row in report["capabilities"]:
            self.assertFalse(row["available"])
            self.assertEqual(row["unavailable_code"], "model_unavailable")

    def test_embedding_operation_requires_positive_locked_dimension(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = entry(catalog_id="nodim", model_id="synthetic/nodim", family="siglip2",
                          operations=("visual_embedding",), dimension=None)
            root = make_fixture_dir(tmp, model)
            with patched_catalog(Path(tmp), [model]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float32")
        row = report["capabilities"][0]
        self.assertFalse(row["available"])
        self.assertEqual(row["unavailable_code"], "model_unavailable")
        self.assertEqual(row["dimension"], 0)

    def test_interval_operations_allow_zero_dimension(self):
        with tempfile.TemporaryDirectory() as tmp:
            events = entry(catalog_id="synthetic-events", model_id="synthetic/model",
                           operations=("sound_events",), dimension=None)
            diarization = entry(catalog_id="synthetic-diarization", model_id="synthetic/diar",
                                operations=("diarization",), dimension=None)
            for model in (events, diarization):
                make_fixture_dir(tmp, model)
            root = Path(tmp) / "models"
            with patched_catalog(Path(tmp), [events, diarization]), patched_dependencies(True):
                report = adapters.capability_report(root, "cpu", "float32")
        for row in report["capabilities"]:
            self.assertTrue(row["available"], row)
            self.assertNotIn("unavailable_code", row)
            self.assertEqual(row["dimension"], 0)


class ZeroDimensionValidationTest(unittest.TestCase):
    """Structured result validation is retained at dimension 0."""

    def test_synthetic_sound_events_validate(self):
        payload = {"kind": "sound_events", "events": [
            {"label": "rain", "start_ms": 0, "end_ms": 900, "raw_score": 0.8, "confidence": None},
        ]}
        self.assertIs(protocol.validate_result("sound_events", payload, 0), payload)

    def test_synthetic_diarization_validates(self):
        payload = {"kind": "diarization", "turns": [
            {"start_ms": 0, "end_ms": 4000, "local_speaker_id": "spk-0",
             "confidence": None, "overlap": False},
        ]}
        self.assertIs(protocol.validate_result("diarization", payload, 0), payload)

    def test_structured_validation_survives_zero_dimension(self):
        with self.assertRaises(protocol.InvalidOutput):
            protocol.validate_result("sound_events", {"kind": "sound_events", "events": [
                {"label": "rain", "start_ms": 900, "end_ms": 0, "raw_score": 0.8,
                 "confidence": None},
            ]}, 0)
        with self.assertRaises(protocol.InvalidOutput):
            protocol.validate_result("diarization", {"kind": "diarization", "turns": [
                {"start_ms": 0, "end_ms": 4000, "local_speaker_id": "spk-0",
                 "confidence": None, "overlap": "maybe"},
            ]}, 0)

    def test_require_positive_dimension_gates_embeddings_only(self):
        with self.assertRaises(adapters.ModelUnavailable):
            adapters.require_positive_dimension("visual_embedding", 0)
        with self.assertRaises(adapters.ModelUnavailable):
            adapters.require_positive_dimension("audio_embedding", 0)
        with self.assertRaises(adapters.ModelUnavailable):
            adapters.require_positive_dimension("speaker_embedding", 0)
        self.assertEqual(adapters.require_positive_dimension("sound_events", 0), 0)
        self.assertEqual(adapters.require_positive_dimension("diarization", 0), 0)
        self.assertEqual(adapters.require_positive_dimension("visual_embedding", 768), 768)


if __name__ == "__main__":
    unittest.main()
