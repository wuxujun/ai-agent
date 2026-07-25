#!/usr/bin/env python3

import json
import os
import sys
import time
import urllib.error
import urllib.request


def load_manifest(path):
    with open(path, encoding="utf-8") as handle:
        models = json.load(handle)
    if not isinstance(models, list) or not models:
        raise ValueError("bootstrap manifest must contain a non-empty model list")
    names = set()
    for model in models:
        if not isinstance(model, dict):
            raise ValueError("every bootstrap model must be an object")
        name = model.get("model_name")
        params = model.get("litellm_params")
        info = model.get("model_info")
        if not isinstance(name, str) or not name.strip():
            raise ValueError("every bootstrap model requires model_name")
        if name in names:
            raise ValueError(f"duplicate bootstrap model_name: {name}")
        if not isinstance(params, dict) or not params.get("model"):
            raise ValueError(f"bootstrap model {name} requires litellm_params.model")
        if not isinstance(info, dict):
            raise ValueError(f"bootstrap model {name} requires model_info")
        names.add(name)
    return models


def request_json(base_url, master_key, method, path, payload=None, timeout=10):
    body = None
    headers = {"Authorization": f"Bearer {master_key}"}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        f"{base_url.rstrip('/')}{path}",
        data=body,
        headers=headers,
        method=method,
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        raw = response.read()
    return json.loads(raw) if raw else {}


def existing_model_names(base_url, master_key, timeout=10):
    response = request_json(base_url, master_key, "GET", "/model/info", timeout=timeout)
    data = response.get("data")
    if not isinstance(data, list):
        raise ValueError("LiteLLM /model/info returned invalid data")
    return {
        item.get("model_name")
        for item in data
        if isinstance(item, dict) and isinstance(item.get("model_name"), str)
    }


def reconcile(base_url, master_key, models, timeout=10):
    existing = existing_model_names(base_url, master_key, timeout)
    created = []
    for model in models:
        name = model["model_name"]
        if name in existing:
            continue
        try:
            request_json(base_url, master_key, "POST", "/model/new", model, timeout)
        except urllib.error.HTTPError:
            # Another reconciler may have created the alias after our list call.
            if name not in existing_model_names(base_url, master_key, timeout):
                raise
        existing.add(name)
        created.append(name)
    return created


def main():
    base_url = os.getenv("LITELLM_BASE_URL", "http://litellm:4000")
    master_key = os.getenv("LITELLM_MASTER_KEY", "")
    manifest = os.getenv(
        "LITELLM_MODEL_BOOTSTRAP_MANIFEST",
        "/bootstrap/bootstrap-models.json",
    )
    interval = int(os.getenv("LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS", "60"))
    timeout = int(os.getenv("LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS", "10"))
    if not master_key:
        raise ValueError("LITELLM_MASTER_KEY is required")
    if interval < 5:
        raise ValueError("LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS must be >= 5")
    if timeout <= 0:
        raise ValueError("LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS must be > 0")

    models = load_manifest(manifest)
    while True:
        try:
            created = reconcile(base_url, master_key, models, timeout)
            if created:
                print("created missing LiteLLM models: " + ", ".join(created), flush=True)
        except (OSError, ValueError, urllib.error.URLError) as error:
            print(f"LiteLLM model bootstrap failed: {error}", file=sys.stderr, flush=True)
        time.sleep(interval)


if __name__ == "__main__":
    main()
