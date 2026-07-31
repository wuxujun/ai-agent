import json
import os
import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
from io import StringIO
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import bootstrap_models


class BootstrapModelsTest(unittest.TestCase):
    def test_main_retries_manifest_load_failure_without_exiting(self):
        class StopLoop(Exception):
            pass

        stderr = StringIO()
        with mock.patch.dict(
            os.environ,
            {
                "LITELLM_MASTER_KEY": "master",
                "LITELLM_MODEL_BOOTSTRAP_MANIFEST": "/bootstrap/qwen-models.json",
                "LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS": "60",
                "LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS": "10",
            },
            clear=True,
        ), mock.patch.object(
            bootstrap_models,
            "load_manifest",
            side_effect=FileNotFoundError("manifest not mounted"),
        ), mock.patch.object(
            bootstrap_models.time,
            "sleep",
            side_effect=StopLoop,
        ), redirect_stderr(stderr):
            with self.assertRaises(StopLoop):
                bootstrap_models.main()

        self.assertIn("/bootstrap/qwen-models.json", stderr.getvalue())
        self.assertIn("manifest not mounted", stderr.getvalue())

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

    def test_reconcile_resolves_team_name_to_team_id(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "ai-agent",
                "litellm_params": {
                    "model": "dashscope/qwen3.7-plus",
                    "litellm_credential_name": "dashscope-qwen",
                },
                "model_info": {"id": None},
            },
        ]
        requests = []

        def request(_base_url, _master_key, method, path, payload=None, timeout=10):
            requests.append((method, path, payload))
            if (method, path) == ("GET", "/team/list"):
                return [{"team_alias": "ai-agent", "team_id": "team-123"}]
            if (method, path) == ("POST", "/model/new"):
                self.assertNotIn("team_name", payload)
                self.assertEqual(payload["model_info"]["team_id"], "team-123")
                return {}
            self.fail(f"unexpected request: {method} {path}")

        with mock.patch.object(
            bootstrap_models,
            "existing_model_names",
            return_value=set(),
        ), mock.patch.object(bootstrap_models, "request_json", side_effect=request):
            created = bootstrap_models.reconcile(
                "http://litellm",
                "master",
                models,
            )

        self.assertEqual(created, ["agent-planner"])
        self.assertEqual(
            [(method, path) for method, path, _payload in requests],
            [("GET", "/team/list"), ("POST", "/model/new")],
        )
        self.assertNotIn("team_id", models[0]["model_info"])

    def test_reconcile_creates_all_deployments_in_new_model_group(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "THINKTOWN",
                "litellm_params": {
                    "model": "qwen3.7-max",
                    "litellm_credential_name": credential,
                },
                "model_info": {"id": None},
            }
            for credential in ("AgentPlan-Qwen", "AgentPlan-Qwen2")
        ]
        created_credentials = []

        def create(_base_url, _master_key, method, path, payload=None, timeout=10):
            self.assertEqual((method, path), ("POST", "/model/new"))
            created_credentials.append(
                payload["litellm_params"]["litellm_credential_name"]
            )
            return {}

        with mock.patch.object(
            bootstrap_models,
            "existing_model_names",
            return_value=set(),
        ), mock.patch.object(
            bootstrap_models,
            "team_ids_by_name",
            return_value={"THINKTOWN": "team-123"},
        ), mock.patch.object(
            bootstrap_models,
            "request_json",
            side_effect=create,
        ):
            created = bootstrap_models.reconcile(
                "http://litellm",
                "master",
                models,
            )

        self.assertEqual(created, ["agent-planner", "agent-planner"])
        self.assertEqual(
            created_credentials,
            ["AgentPlan-Qwen", "AgentPlan-Qwen2"],
        )

    def test_reconcile_ignores_empty_team_name(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "   ",
                "litellm_params": {"model": "openai/model"},
                "model_info": {"id": None},
            },
        ]
        payloads = []

        def create(_base_url, _master_key, method, path, payload=None, timeout=10):
            self.assertEqual((method, path), ("POST", "/model/new"))
            payloads.append(payload)
            return {}

        with mock.patch.object(
            bootstrap_models,
            "existing_model_names",
            return_value=set(),
        ), mock.patch.object(
            bootstrap_models,
            "team_ids_by_name",
        ) as list_teams, mock.patch.object(
            bootstrap_models,
            "request_json",
            side_effect=create,
        ):
            created = bootstrap_models.reconcile(
                "http://litellm",
                "master",
                models,
            )

        self.assertEqual(created, ["agent-planner"])
        list_teams.assert_not_called()
        self.assertNotIn("team_name", payloads[0])
        self.assertNotIn("team_id", payloads[0]["model_info"])

    def test_reconcile_rejects_unknown_team_name(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "missing-team",
                "litellm_params": {"model": "dashscope/qwen3.7-plus"},
                "model_info": {"id": None},
            },
        ]
        with mock.patch.object(
            bootstrap_models,
            "existing_model_names",
            return_value=set(),
        ), mock.patch.object(
            bootstrap_models,
            "team_ids_by_name",
            return_value={"ai-agent": "team-123"},
        ):
            with self.assertRaisesRegex(ValueError, "missing-team"):
                bootstrap_models.reconcile("http://litellm", "master", models)

    def test_existing_model_names_includes_team_public_name(self):
        with mock.patch.object(
            bootstrap_models,
            "request_json",
            return_value={
                "data": [
                    {
                        "model_name": "model_name_team-123_generated",
                        "model_info": {
                            "team_public_model_name": "agent-planner",
                        },
                    },
                ],
            },
        ):
            names = bootstrap_models.existing_model_names(
                "http://litellm",
                "master",
            )
        self.assertEqual(
            names,
            {"model_name_team-123_generated", "agent-planner"},
        )

    def test_manifest_rejects_duplicate_deployments(self):
        models = [
            {
                "model_name": "duplicate",
                "team_name": "team",
                "litellm_params": {
                    "model": "openai/model",
                    "litellm_credential_name": "credential",
                },
                "model_info": {"id": None},
            },
            {
                "model_name": "duplicate",
                "team_name": "team",
                "litellm_params": {
                    "model": "openai/model",
                    "litellm_credential_name": "credential",
                },
                "model_info": {"id": None},
            },
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            with self.assertRaisesRegex(ValueError, "duplicate bootstrap deployment"):
                bootstrap_models.load_manifest(path)
        finally:
            os.unlink(path)

    def test_manifest_allows_model_group_with_distinct_credentials(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "THINKTOWN",
                "litellm_params": {
                    "model": "qwen3.7-max",
                    "litellm_credential_name": credential,
                },
                "model_info": {"id": None},
            }
            for credential in ("AgentPlan-Qwen", "AgentPlan-Qwen2")
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            self.assertEqual(bootstrap_models.load_manifest(path), models)
        finally:
            os.unlink(path)

    def test_manifest_rejects_empty_credential_name(self):
        models = [
            {
                "model_name": "agent-planner",
                "litellm_params": {
                    "model": "dashscope/qwen3.7-plus",
                    "litellm_credential_name": " ",
                },
                "model_info": {"id": None},
            },
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            with self.assertRaisesRegex(ValueError, "litellm_credential_name"):
                bootstrap_models.load_manifest(path)
        finally:
            os.unlink(path)

    def test_manifest_allows_empty_team_name_with_team_id(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "",
                "litellm_params": {"model": "dashscope/qwen3.7-plus"},
                "model_info": {"id": None, "team_id": "team-123"},
            },
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            self.assertEqual(bootstrap_models.load_manifest(path), models)
        finally:
            os.unlink(path)

    def test_manifest_rejects_nonempty_team_name_with_team_id(self):
        models = [
            {
                "model_name": "agent-planner",
                "team_name": "ai-agent",
                "litellm_params": {"model": "dashscope/qwen3.7-plus"},
                "model_info": {"id": None, "team_id": "team-123"},
            },
        ]
        with tempfile.NamedTemporaryFile("w", delete=False) as handle:
            json.dump(models, handle)
            path = handle.name
        try:
            with self.assertRaisesRegex(ValueError, "cannot set both"):
                bootstrap_models.load_manifest(path)
        finally:
            os.unlink(path)

    def test_qwen_manifest_uses_named_litellm_credential(self):
        path = pathlib.Path(__file__).resolve().parent / "qwen-models.json"
        models = bootstrap_models.load_manifest(path)
        self.assertGreater(len(models), 0)
        for model in models:
            self.assertIsInstance(model["team_name"], str)
            self.assertTrue(model["team_name"])
            params = model["litellm_params"]
            self.assertIsInstance(params["litellm_credential_name"], str)
            self.assertTrue(params["litellm_credential_name"])
            self.assertNotIn("api_key", params)
            self.assertNotIn("api_base", params)

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
                "agent-embedding",
                "agent-planner-legacy",
                "agent-planner-fallback-legacy",
                "agent-writer-legacy",
                "agent-fast-legacy",
            },
        )
        qwen_models = [
            model for model in models
            if not model["model_name"].endswith("-legacy")
            and model["model_name"] != "agent-embedding"
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

        embedding = next(
            model for model in models
            if model["model_name"] == "agent-embedding"
        )
        self.assertEqual(
            embedding["litellm_params"],
            {
                "model": "dashscope/text-embedding-v4",
                "api_key": "os.environ/DASHSCOPE_API_KEY",
                "api_base": "os.environ/DASHSCOPE_API_BASE",
            },
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
