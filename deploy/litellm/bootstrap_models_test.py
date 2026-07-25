import json
import os
import pathlib
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import bootstrap_models


class BootstrapModelsTest(unittest.TestCase):
    def test_reconcile_creates_only_missing_models_and_is_idempotent(self):
        models = [
            {
                "model_name": "agent-planner",
                "litellm_params": {"model": "openai/model"},
                "model_info": {"id": None},
            },
            {
                "model_name": "agent-writer",
                "litellm_params": {"model": "openai/model"},
                "model_info": {"id": None},
            },
        ]
        existing = {"agent-planner"}
        created_by_api = []

        def create(_base_url, _master_key, method, path, payload=None, timeout=10):
            self.assertEqual((method, path), ("POST", "/model/new"))
            self.assertEqual(timeout, 10)
            existing.add(payload["model_name"])
            created_by_api.append(payload["model_name"])
            return {}

        with mock.patch.object(
            bootstrap_models,
            "existing_model_names",
            side_effect=lambda *_args: set(existing),
        ), mock.patch.object(bootstrap_models, "request_json", side_effect=create):
            created = bootstrap_models.reconcile("http://litellm", "master", models)
            self.assertEqual(created, ["agent-writer"])
            created = bootstrap_models.reconcile("http://litellm", "master", models)
            self.assertEqual(created, [])
        self.assertEqual(created_by_api, ["agent-writer"])

    def test_manifest_rejects_duplicate_names(self):
        models = [
            {
                "model_name": "duplicate",
                "litellm_params": {"model": "openai/model"},
                "model_info": {"id": None},
            },
            {
                "model_name": "duplicate",
                "litellm_params": {"model": "openai/other"},
                "model_info": {"id": None},
            },
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            with self.assertRaisesRegex(ValueError, "duplicate"):
                bootstrap_models.load_manifest(path)
        finally:
            os.unlink(path)

    def test_seed_manifest_uses_distinct_dashscope_models(self):
        path = pathlib.Path(__file__).resolve().parent / "bootstrap-models.json"
        models = bootstrap_models.load_manifest(path)
        self.assertEqual(
            {model["model_name"] for model in models},
            {
                "agent-planner",
                "agent-planner-fallback",
                "agent-writer",
                "agent-fast",
                "agent-planner-legacy",
                "agent-planner-fallback-legacy",
                "agent-writer-legacy",
                "agent-fast-legacy",
            },
        )
        qwen_models = [
            model for model in models
            if not model["model_name"].endswith("-legacy")
        ]
        upstream_models = {
            model["litellm_params"]["model"] for model in qwen_models
        }
        self.assertEqual(len(upstream_models), 4)
        for model in qwen_models:
            params = model["litellm_params"]
            self.assertTrue(params["model"].startswith("dashscope/qwen"))
            self.assertEqual(
                params["api_key"],
                "os.environ/DASHSCOPE_API_KEY",
            )
            self.assertEqual(
                params["api_base"],
                "os.environ/DASHSCOPE_API_BASE",
            )

        legacy = {
            model["model_name"]: model["litellm_params"]
            for model in models
            if model["model_name"].endswith("-legacy")
        }
        self.assertEqual(
            legacy["agent-planner-legacy"]["model"],
            "openai/gpt-4.1-mini",
        )
        self.assertEqual(
            legacy["agent-planner-fallback-legacy"]["model"],
            "gemini/gemini-2.5-flash",
        )
        self.assertEqual(
            legacy["agent-writer-legacy"]["model"],
            "openai/gpt-4.1-mini",
        )
        self.assertEqual(
            legacy["agent-fast-legacy"]["model"],
            "gemini/gemini-2.5-flash",
        )


if __name__ == "__main__":
    unittest.main()
