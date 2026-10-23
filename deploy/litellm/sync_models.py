#!/usr/bin/env python3
"""Sync local manifests once using the running LiteLLM container's credentials."""

import argparse
import inspect
import json
from pathlib import Path
import subprocess
import sys

import bootstrap_models


def sync_once(bootstrap, models):
    import collections
    import os
    from urllib.parse import quote

    base_url = "http://127.0.0.1:4000"
    master_key = os.environ.get("LITELLM_MASTER_KEY", "")
    if not master_key:
        raise ValueError("LITELLM_MASTER_KEY is missing in the LiteLLM container")
    timeout = 20
    credentials = {
        model["litellm_params"].get("litellm_credential_name") for model in models
    }
    for credential in sorted(value for value in credentials if value):
        bootstrap.request_json(
            base_url, master_key, "GET",
            "/credentials/by_name/" + quote(credential, safe=""), timeout=timeout,
        )
    created, updated = bootstrap.reconcile(base_url, master_key, models, timeout)
    existing = bootstrap.existing_models(base_url, master_key, timeout)
    counts = collections.Counter(model["model_name"] for model in models)
    claimed = set()
    for model in models:
        candidates = [
            item for item in existing
            if bootstrap.effective_model_name(item) == model["model_name"]
            and id(item) not in claimed
        ]
        match = bootstrap.select_existing_model(
            model, candidates, allow_single_fallback=counts[model["model_name"]] == 1,
        )
        if match is None or bootstrap.litellm_params_need_update(
            match, model["litellm_params"],
        ):
            raise ValueError("Saved model does not match the manifest")
        claimed.add(id(match))
    return {"created": created, "updated": updated, "verified": len(models)}


def remote_script(models):
    # Send code and manifests over stdin: no extra mounts or secret arguments.
    source = Path(bootstrap_models.__file__).read_text(encoding="utf-8")
    return (
        "import json, sys, types, urllib.error\n"
        "bootstrap = types.ModuleType('bootstrap_models')\n"
        f"exec(compile({source!r}, 'bootstrap_models.py', 'exec'), bootstrap.__dict__)\n"
        + inspect.getsource(sync_once)
        + f"\nmodels = json.loads({json.dumps(models)!r})\n"
        "try:\n"
        "    print(json.dumps(sync_once(bootstrap, models)))\n"
        "except urllib.error.HTTPError as error:\n"
        "    print('LiteLLM synchronization failed: HTTP ' + str(error.code), file=sys.stderr)\n"
        "    sys.exit(1)\n"
        "except Exception as error:\n"
        "    print('LiteLLM synchronization or verification failed: ' + type(error).__name__, file=sys.stderr)\n"
        "    sys.exit(1)\n"
    )


def main(argv=None):
    directory = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifests", nargs="+", help="JSON paths relative to the current directory")
    parser.add_argument("--env-file", default=str(directory / ".env"), help="Compose environment file")
    args = parser.parse_args(argv)
    try:
        models = bootstrap_models.load_manifests(args.manifests)
        # Reject duplicate declarations across files before any API writes.
        deployments = set()
        for model in models:
            params = model["litellm_params"]
            identity = (
                model["model_name"], params["model"],
                params.get("litellm_credential_name"),
                bootstrap_models.configured_team_name(model), model["model_info"].get("team_id"),
            )
            if identity in deployments:
                raise ValueError("Duplicate deployment across manifests")
            deployments.add(identity)
        result = subprocess.run(
            [
                "docker", "compose", "--env-file", args.env_file,
                "-f", str(directory / "compose.yaml"),
                "exec", "-T", "litellm", "python", "-",
            ],
            input=remote_script(models), text=True, check=False,
        )
        return result.returncode
    except (OSError, ValueError) as error:
        # Do not print JSON contents, credentials, or upstream response bodies.
        print("Cannot synchronize models: " + type(error).__name__ +
              "; check manifest paths, JSON schema, and Docker availability.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
