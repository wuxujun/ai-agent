import copy
from contextlib import redirect_stderr
from io import StringIO
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import bootstrap_models
import sync_models


class SyncModelsTest(unittest.TestCase):
    def setUp(self):
        self.models = [{
            "model_name": "alias",
            "litellm_params": {"model": "new-model", "litellm_credential_name": "credential"},
            "model_info": {"id": None},
        }]

    def test_sync_updates_and_verifies_using_container_credentials(self):
        saved = copy.deepcopy(self.models)
        saved[0]["model_info"]["id"] = "deployment"
        with mock.patch.dict("os.environ", {"LITELLM_MASTER_KEY": "secret"}), \
             mock.patch.object(bootstrap_models, "request_json") as request, \
             mock.patch.object(bootstrap_models, "reconcile", return_value=([], ["alias"])) as reconcile, \
             mock.patch.object(bootstrap_models, "existing_models", return_value=saved):
            result = sync_models.sync_once(bootstrap_models, self.models)
        self.assertEqual(result, {"created": [], "updated": ["alias"], "verified": 1})
        request.assert_called_once_with(
            "http://127.0.0.1:4000", "secret", "GET",
            "/credentials/by_name/credential", timeout=20,
        )
        reconcile.assert_called_once()

    def test_sync_fails_when_saved_configuration_does_not_match(self):
        with mock.patch.dict("os.environ", {"LITELLM_MASTER_KEY": "secret"}), \
             mock.patch.object(bootstrap_models, "request_json"), \
             mock.patch.object(bootstrap_models, "reconcile", return_value=(["alias"], [])), \
             mock.patch.object(bootstrap_models, "existing_models", return_value=[]):
            with self.assertRaisesRegex(ValueError, "does not match"):
                sync_models.sync_once(bootstrap_models, self.models)

    def test_missing_credential_prevents_model_writes(self):
        with mock.patch.dict("os.environ", {"LITELLM_MASTER_KEY": "secret"}), \
             mock.patch.object(bootstrap_models, "request_json", side_effect=ValueError), \
             mock.patch.object(bootstrap_models, "reconcile") as reconcile:
            with self.assertRaises(ValueError):
                sync_models.sync_once(bootstrap_models, self.models)
        reconcile.assert_not_called()

    def test_cli_passes_manifests_over_stdin_and_propagates_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "models with spaces.json"
            path.write_text(json.dumps(self.models))
            with mock.patch.object(sync_models.subprocess, "run") as run:
                run.return_value.returncode = 1
                self.assertEqual(sync_models.main([str(path)]), 1)
            command = run.call_args.args[0]
            self.assertEqual(command[-5:], ["exec", "-T", "litellm", "python", "-"])
            self.assertNotIn("secret", " ".join(command))
            compile(run.call_args.kwargs["input"], "remote", "exec")
            with mock.patch.object(sync_models.subprocess, "run") as run, redirect_stderr(StringIO()):
                self.assertEqual(sync_models.main([str(path), str(path)]), 1)
                run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
