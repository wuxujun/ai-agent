#!/usr/bin/env python3

import copy
import json
import os
import sys
import time
import urllib.error
import urllib.request


HIDDEN_SECRET_PARAMS = {
    "api_key",
    "azure_ad_token",
    "aws_access_key_id",
    "aws_secret_access_key",
    "vertex_credentials",
}


def configured_team_name(model):
    team_name = model.get("team_name")
    if team_name is None:
        return None
    if not isinstance(team_name, str):
        raise ValueError("team_name must be a string or null")
    team_name = team_name.strip()
    return team_name or None


def load_manifest(path):
    with open(path, encoding="utf-8") as handle:
        models = json.load(handle)
    if not isinstance(models, list) or not models:
        raise ValueError("bootstrap manifest must contain a non-empty model list")
    deployments = set()
    for model in models:
        if not isinstance(model, dict):
            raise ValueError("every bootstrap model must be an object")
        name = model.get("model_name")
        params = model.get("litellm_params")
        info = model.get("model_info")
        if not isinstance(name, str) or not name.strip():
            raise ValueError("every bootstrap model requires model_name")
        if not isinstance(params, dict) or not params.get("model"):
            raise ValueError(f"bootstrap model {name} requires litellm_params.model")
        credential_name = params.get("litellm_credential_name")
        if credential_name is not None and (
            not isinstance(credential_name, str)
            or (credential_name != "" and not credential_name.strip())
        ):
            raise ValueError(
                f"bootstrap model {name} has invalid "
                "litellm_params.litellm_credential_name"
            )
        if not isinstance(info, dict):
            raise ValueError(f"bootstrap model {name} requires model_info")
        try:
            team_name = configured_team_name(model)
        except ValueError as error:
            raise ValueError(f"bootstrap model {name} has invalid team_name") from error
        if team_name is not None and info.get("team_id") is not None:
            raise ValueError(
                f"bootstrap model {name} cannot set both team_name "
                "and model_info.team_id"
            )
        deployment = (
            name,
            params.get("model"),
            credential_name,
            team_name,
            info.get("team_id"),
        )
        if deployment in deployments:
            raise ValueError(
                f"duplicate bootstrap deployment for model_name: {name}"
            )
        deployments.add(deployment)
    return models


def configured_manifest_paths():
    multiple = os.getenv("LITELLM_MODEL_BOOTSTRAP_MANIFESTS", "")
    if multiple.strip():
        paths = [path.strip() for path in multiple.split(",") if path.strip()]
    else:
        paths = [
            os.getenv(
                "LITELLM_MODEL_BOOTSTRAP_MANIFEST",
                "/bootstrap/bootstrap-models.json",
            ).strip()
        ]
    if not paths or any(not path for path in paths):
        raise ValueError("at least one bootstrap manifest path is required")
    return paths


def load_manifests(paths):
    models = []
    for path in paths:
        models.extend(load_manifest(path))
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


def describe_error(error):
    if not isinstance(error, urllib.error.HTTPError):
        return str(error)
    body = error.read(4096)
    detail = body.decode("utf-8", errors="replace").strip()
    message = f"HTTP {error.code} {error.reason} from {error.url}"
    if detail:
        message += f": {detail}"
    return message


def existing_models(base_url, master_key, timeout=10):
    response = request_json(base_url, master_key, "GET", "/model/info", timeout=timeout)
    data = response.get("data")
    if not isinstance(data, list):
        raise ValueError("LiteLLM /model/info returned invalid data")
    return [item for item in data if isinstance(item, dict)]


def effective_model_name(model):
    info = model.get("model_info")
    if isinstance(info, dict):
        public_name = info.get("team_public_model_name")
        if isinstance(public_name, str) and public_name:
            return public_name
    name = model.get("model_name")
    return name if isinstance(name, str) else None


def existing_model_names(base_url, master_key, timeout=10):
    names = set()
    for item in existing_models(base_url, master_key, timeout):
        raw_name = item.get("model_name")
        if isinstance(raw_name, str):
            names.add(raw_name)
        effective_name = effective_model_name(item)
        if isinstance(effective_name, str):
            names.add(effective_name)
    return names


def team_ids_by_name(base_url, master_key, timeout=10):
    response = request_json(base_url, master_key, "GET", "/team/list", timeout=timeout)
    if isinstance(response, list):
        teams = response
    elif isinstance(response, dict):
        teams = response.get("teams", response.get("data"))
    else:
        teams = None
    if not isinstance(teams, list):
        raise ValueError("LiteLLM /team/list returned invalid data")

    result = {}
    for team in teams:
        if not isinstance(team, dict):
            continue
        name = team.get("team_alias")
        team_id = team.get("team_id")
        if not isinstance(name, str) or not isinstance(team_id, str):
            continue
        if name in result and result[name] != team_id:
            raise ValueError(f"duplicate LiteLLM team_alias: {name}")
        result[name] = team_id
    return result


def model_create_payload(model, team_ids):
    payload = copy.deepcopy(model)
    team_name_configured = "team_name" in payload
    team_name = configured_team_name(payload)
    payload.pop("team_name", None)
    if team_name is None:
        # An explicitly empty/null team_name opts out of Team scoping even if
        # a stale model_info.team_id remains in the manifest.
        if team_name_configured:
            payload["model_info"].pop("team_id", None)
        return payload
    team_id = team_ids.get(team_name)
    if team_id is None:
        raise ValueError(f"LiteLLM team not found: {team_name}")
    payload["model_info"]["team_id"] = team_id
    return payload


def select_existing_model(model, candidates, allow_single_fallback=True):
    if not candidates:
        return None
    desired_params = model["litellm_params"]
    desired_model = desired_params.get("model")
    desired_credential = desired_params.get("litellm_credential_name")
    exact = []
    for candidate in candidates:
        params = candidate.get("litellm_params")
        if not isinstance(params, dict):
            continue
        if (
            params.get("model") == desired_model
            and params.get("litellm_credential_name") == desired_credential
        ):
            exact.append(candidate)
    if len(exact) == 1:
        return exact[0]
    if len(exact) > 1:
        raise ValueError(
            f"multiple existing LiteLLM deployments match {model['model_name']}"
        )
    if allow_single_fallback and len(candidates) == 1:
        return candidates[0]
    if allow_single_fallback:
        raise ValueError(
            f"cannot uniquely match existing LiteLLM deployment: "
            f"{model['model_name']}"
        )
    return None


def litellm_params_need_update(existing, desired):
    current = existing.get("litellm_params")
    if not isinstance(current, dict):
        return True
    for key, value in desired.items():
        if (
            key in HIDDEN_SECRET_PARAMS
            and key not in current
            and isinstance(value, str)
            and value.startswith("os.environ/")
        ):
            # LiteLLM intentionally omits stored secrets from /model/info, so
            # their absence cannot be treated as a configuration difference.
            continue
        if current.get(key) != value:
            return True
    return False


def model_id(existing):
    info = existing.get("model_info")
    value = info.get("id") if isinstance(info, dict) else None
    if not isinstance(value, str) or not value:
        raise ValueError(
            f"existing LiteLLM model has no model_info.id: "
            f"{effective_model_name(existing)}"
        )
    return value


def reconcile(base_url, master_key, models, timeout=10):
    current_models = existing_models(base_url, master_key, timeout)
    by_name = {}
    for current in current_models:
        name = effective_model_name(current)
        if name is not None:
            by_name.setdefault(name, []).append(current)

    manifest_counts = {}
    for model in models:
        manifest_counts[model["model_name"]] = (
            manifest_counts.get(model["model_name"], 0) + 1
        )
    team_ids = {}
    if any(configured_team_name(model) is not None for model in models):
        team_ids = team_ids_by_name(base_url, master_key, timeout)
    created = []
    updated = []
    claimed = set()
    for model in models:
        name = model["model_name"]
        candidates = [
            candidate
            for candidate in by_name.get(name, [])
            if id(candidate) not in claimed
        ]
        existing = select_existing_model(
            model,
            candidates,
            allow_single_fallback=manifest_counts[name] == 1,
        )
        if existing is not None:
            claimed.add(id(existing))
            desired_params = copy.deepcopy(model["litellm_params"])
            if litellm_params_need_update(existing, desired_params):
                request_json(
                    base_url,
                    master_key,
                    "PATCH",
                    f"/model/{model_id(existing)}/update",
                    {"litellm_params": desired_params},
                    timeout,
                )
                existing["litellm_params"] = desired_params
                updated.append(name)
            continue

        payload = model_create_payload(model, team_ids)
        try:
            request_json(base_url, master_key, "POST", "/model/new", payload, timeout)
        except urllib.error.HTTPError:
            # Another reconciler may have created the alias after our list call.
            refreshed = [
                item
                for item in existing_models(base_url, master_key, timeout)
                if effective_model_name(item) == name
            ]
            if not refreshed:
                raise
            by_name[name] = refreshed
            continue
        created.append(name)
    return created, updated


def main():
    base_url = os.getenv("LITELLM_BASE_URL", "http://litellm:4000")
    master_key = os.getenv("LITELLM_MASTER_KEY", "")
    manifests = configured_manifest_paths()
    manifest_label = ",".join(manifests)
    interval = int(os.getenv("LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS", "60"))
    timeout = int(os.getenv("LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS", "10"))
    if not master_key:
        raise ValueError("LITELLM_MASTER_KEY is required")
    if interval < 5:
        raise ValueError("LITELLM_MODEL_BOOTSTRAP_INTERVAL_SECONDS must be >= 5")
    if timeout <= 0:
        raise ValueError("LITELLM_MODEL_BOOTSTRAP_TIMEOUT_SECONDS must be > 0")

    while True:
        try:
            # Load on every pass so a temporarily missing/invalid bind mount
            # does not terminate the container and corrected manifests are
            # picked up without recreating the reconciler.
            models = load_manifests(manifests)
            created, updated = reconcile(base_url, master_key, models, timeout)
            if created:
                print("created missing LiteLLM models: " + ", ".join(created), flush=True)
            if updated:
                print("updated LiteLLM models: " + ", ".join(updated), flush=True)
        except (OSError, ValueError, urllib.error.URLError) as error:
            print(
                f"LiteLLM model bootstrap failed for {manifest_label}: "
                f"{describe_error(error)}",
                file=sys.stderr,
                flush=True,
            )
        time.sleep(interval)


if __name__ == "__main__":
    main()
