#!/usr/bin/env python3
"""Render private, test-only guest assets; never modifies repository config."""
import argparse
import base64
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil

KIT = Path(__file__).resolve().parent
ROLES = ("node-a", "node-b", "storage", "control")


def topology(path):
    cfg = json.loads(Path(path).read_text())
    prefix = cfg["prefix"]
    if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", prefix) or len(prefix) > 50:
        raise ValueError("invalid prefix")
    net = ipaddress.IPv4Network(cfg["cidr"], strict=True)
    private = any(net.subnet_of(ipaddress.ip_network(n)) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
    if net.prefixlen != 24 or not private or set(cfg["nodes"]) != set(ROLES):
        raise ValueError("four roles and an RFC1918 /24 required")
    addresses = [ipaddress.IPv4Address(cfg["nodes"][r]) for r in ROLES]
    if len(set(addresses)) != 4 or any(a not in net or int(a) - int(net.network_address) not in range(2, 255) for a in addresses):
        raise ValueError("invalid node addresses")
    return cfg


def new_path(path):
    original = Path(path)
    if original.is_symlink():
        raise ValueError("output must not be a symlink")
    path = original.resolve()
    repo = KIT.parents[2]
    if path == repo or repo in path.parents:
        raise ValueError("private output must be outside the repository")
    if path.exists() or path.is_symlink():
        raise ValueError("output already exists")
    return path


def write(path, text):
    with open(path, "x", encoding="utf-8") as stream:
        stream.write(text)
    os.chmod(path, 0o600)


def dump(value):
    return json.dumps(value, indent=2, ensure_ascii=False) + "\n"


def env(values):
    # JSON string quotes are compatible with these single-line systemd values.
    return "".join(f"{k}={json.dumps(str(v))}\n" for k, v in values.items())


def render(cfg, secret, output):
    for key in ("api_admin_key", "api_tenant_key", "postgres_password", "redis_password", "stub_token"):
        if not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", secret[key]):
            raise ValueError("secrets must be 32-128 URL-safe characters")
    if len(base64.b64decode(secret["approval_key"], validate=True)) != 32:
        raise ValueError("approval key must encode 32 bytes")
    if len(set(secret.values())) != len(secret):
        raise ValueError("use distinct secrets")
    output.mkdir(mode=0o700)
    nodes = cfg["nodes"]
    storage, control = nodes["storage"], nodes["control"]
    dsn = f"postgres://ai_agent_ha:{secret['postgres_password']}@{storage}:15432/ai_agent_ha?sslmode=disable"
    redis = f"redis://:{secret['redis_password']}@{storage}:16379/0"
    base = f"http://{control}:18080/v1/chat/completions"
    for role in ROLES:
        dest = output / role
        dest.mkdir(mode=0o700)
        write(dest / "bootstrap.env", env({"HA_PREFIX": cfg["prefix"], "HA_ROLE": role,
            **{"HA_" + r.upper().replace("-", "_") + "_IP": nodes[r] for r in ROLES}}))
    for role in ("node-a", "node-b"):
        dest = output / role
        config = {
            "api": {"addr": "0.0.0.0:8088", "auth": {"mode": "api_key", "require_tenant_workspace_root": True},
                "tenants": {"ha_test": {"api_key": secret["api_tenant_key"], "admin": False,
                    "workspace_root": "/opt/ai-agent/workspace/ha-fixture", "default_multiagent_team": "software",
                    "allowed_multiagent_teams": ["software"], "daily_llm_call_budget": 5000, "daily_llm_cost_budget_usd": 20}}},
            "store": {"type": "postgres", "vector_search": "in_process", "postgres": {"max_open_conns": 50, "max_idle_conns": 10}},
            "orchestrator": {"mode": "multiagent", "max_concurrent_tasks": 4},
            "multiagent": {"team": "software", "runtime": "legacy", "dag_canary_percent": 0},
            "llm": {"provider": "litellm", "api_key": "", "model": "ha-offline", "base_url": base, "readiness_mode": "config_only",
                "max_calls_per_task": 12, "max_estimated_cost_usd_per_task": 0.05,
                "gateway": {"provider": "litellm", "model": "ha-offline", "base_url": base,
                    "input_cost_per_million_usd": 0.01, "output_cost_per_million_usd": 0.01},
                "scenes": {"embedding": {"provider": "litellm", "model": "ha-embedding", "base_url": f"http://{control}:18080/v1/embeddings"}}},
            "log": {"level": "info", "console": True, "file_enabled": True, "access_enabled": True,
                "directory": "/opt/ai-agent/logs", "retention_days": 30},
            "telemetry": {"enabled": True, "endpoint": "127.0.0.1:4318", "exporter": "otlp", "environment": "ha-test"},
            "answer_pipeline": {"enabled": False}, "langfuse": {"enabled": False}, "brain": {"enabled": False},
        }
        write(dest / "ha-config.json", dump(config))
        write(dest / "ha-fixture.conf", """[Unit]
RequiresMountsFor=/opt/ai-agent/workspace
[Service]
WorkingDirectory=/opt/ai-agent
BindReadOnlyPaths=/opt/ai-agent-ha/runtime/teams.yaml:/opt/ai-agent/teams.yaml
""")
        write(dest / "otel-collector.json", dump({
            "receivers": {"otlp": {"protocols": {"http": {"endpoint": "0.0.0.0:4318"}}}},
            "processors": {"batch": {}},
            "exporters": {"prometheus": {"endpoint": "0.0.0.0:9464"}, "debug": {"verbosity": "basic"}},
            "service": {"pipelines": {
                "metrics": {"receivers": ["otlp"], "processors": ["batch"], "exporters": ["prometheus"]},
                "traces": {"receivers": ["otlp"], "processors": ["batch"], "exporters": ["debug"]}}}}))
        write(dest / "collector-compose.json", dump({"name": "ai-agent-ha-telemetry", "services": {
            "otel-collector": {"image": "otel/opentelemetry-collector-contrib:0.162.0",
                "command": ["--config=/etc/otelcol/config.json"], "restart": "unless-stopped",
                "ports": ["127.0.0.1:4318:4318", f"{nodes[role]}:9464:9464"],
                "volumes": ["/etc/ai-agent-ha/otel-collector.json:/etc/otelcol/config.json:ro"],
                "read_only": True, "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"],
                "mem_limit": "512m", "cpus": 1}}}))
        write(dest / "teams.yaml", dump({"active_team": "software", "resume_config_policy": "require_match", "teams": {
            "software": {"lifecycle": "active", "runtime": "legacy", "workflow": "planner_researcher_writer",
                "planner": {"name": "HA fixture planner", "tools": ["read_file"], "llm_scene": "multiagent_planner",
                    "system_prompt": "Read only README.md in the dedicated HA workspace. Return the requested JSON schema."},
                "researcher": {"name": "HA fixture reader"},
                "writer": {"name": "HA fixture writer", "llm_scene": "multiagent_writer",
                    "system_prompt": "Summarize only the fixed HA fixture evidence. Return the requested JSON schema."}}}}))
        write(dest / "ai-agent.env", env({"AI_AGENT_CONFIG_FILE": "/etc/ai-agent/ha-config.json",
            "AI_AGENT_API_KEY": secret["api_admin_key"], "AI_AGENT_STORE_DSN": dsn,
            "AI_AGENT_REDIS_BUS_URL": redis, "AI_AGENT_APPROVAL_ENCRYPTION_KEY": secret["approval_key"],
            "AI_AGENT_LLM_API_KEY": secret["stub_token"], "AI_AGENT_MULTIAGENT_RUNTIME": "legacy",
            "AI_AGENT_MULTIAGENT_DAG_CANARY_PERCENT": 0}))
    dest = output / "storage"
    write(dest / "storage.env", f"HA_POSTGRES_PASSWORD={secret['postgres_password']}\n")
    write(dest / "redis.conf", f"bind 0.0.0.0\nprotected-mode yes\nappendonly yes\nrequirepass {secret['redis_password']}\n")
    write(dest / "compose.yaml", f"""name: ai-agent-ha-storage
services:
  postgres:
    image: pgvector/pgvector:0.8.2-pg17-bookworm
    command: [postgres, -c, max_connections=150]
    environment:
      POSTGRES_DB: ai_agent_ha
      POSTGRES_USER: ai_agent_ha
      POSTGRES_PASSWORD: ${{HA_POSTGRES_PASSWORD:?required}}
    ports: ["{storage}:15432:5432"]
    volumes: ["postgres-data:/var/lib/postgresql/data"]
    healthcheck:
      test: [CMD-SHELL, "pg_isready -U ai_agent_ha -d ai_agent_ha"]
      interval: 5s
      timeout: 5s
      retries: 12
  redis:
    image: redis:7.4.2-alpine
    command: [redis-server, /usr/local/etc/redis/redis.conf]
    ports: ["{storage}:16379:6379"]
    volumes:
      - redis-data:/data
      - ./redis.conf:/usr/local/etc/redis/redis.conf:ro
    healthcheck:
      test: [CMD-SHELL, "redis-cli ping 2>&1 | grep -Eq 'PONG|NOAUTH'"]
      interval: 5s
      timeout: 5s
      retries: 12
volumes:
  postgres-data:
  redis-data:
""")
    clients = " ".join(f"{nodes[r]}(rw,sync,root_squash,no_subtree_check,fsid=0)" for r in ("node-a", "node-b", "control"))
    write(dest / "exports", f"/srv/ai-agent-ha/workspace {clients}\n")
    ganesha_clients = ", ".join(nodes[r] for r in ("node-a", "node-b", "control"))
    write(dest / "ganesha.conf", f"""NFS_CORE_PARAM {{
  Protocols = 4;
  NFS_Port = 2049;
  Bind_addr = {storage};
  Enable_NLM = false;
  Enable_UDP = false;
}}
EXPORT {{
  Export_Id = 88;
  Path = /srv/ai-agent-ha/workspace;
  Pseudo = /;
  Access_Type = None;
  Protocols = 4;
  Transports = TCP;
  SecType = sys;
  Squash = root_squash;
  FSAL {{ Name = VFS; }}
  CLIENT {{
    Clients = {ganesha_clients};
    Access_Type = RW;
    Squash = root_squash;
  }}
}}
""")
    dest = output / "control"
    write(dest / "control.env", env({"AI_AGENT_HA_API_KEY": secret["api_tenant_key"], "TEST_POSTGRES_DSN": dsn,
        "TEST_REDIS_URL": redis.rsplit("/", 1)[0] + "/15", "AI_AGENT_RUN_EXTERNAL_INTEGRATION": "true"}))
    write(dest / "stub.env", env({"HA_STUB_TOKEN": secret["stub_token"], "HA_STUB_ADDR": control}))
    write(dest / "metrics.key", secret["api_admin_key"])
    write(dest / "prometheus.json", dump({"global": {"scrape_interval": "15s"}, "scrape_configs": [{
        "job_name": "ai-agent-ha", "metrics_path": "/metrics",
        "static_configs": [{"targets": [f"{nodes[r]}:9464"], "labels": {"ha_node": r}} for r in ("node-a", "node-b")]}]}))
    write(dest / "compose.yaml", """name: ai-agent-ha-monitor
services:
  prometheus:
    image: prom/prometheus:v3.5.5
    ports: ["127.0.0.1:9090:9090"]
    volumes:
      - ./prometheus.json:/etc/prometheus/prometheus.yml:ro
      - ./metrics.key:/etc/prometheus/metrics.key:ro
      - prometheus-data:/prometheus
volumes:
  prometheus-data:
""")
    shutil.copyfile(KIT / "offline_stub.py", dest / "offline_stub.py")
    os.chmod(dest / "offline_stub.py", 0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    gen = sub.add_parser("secrets")
    gen.add_argument("--output", required=True)
    assets = sub.add_parser("assets")
    assets.add_argument("--config", required=True)
    assets.add_argument("--secrets", required=True)
    assets.add_argument("--output", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    out = new_path(args.output)
    if args.action == "secrets":
        data = {k: secrets.token_hex(32) for k in ("api_admin_key", "api_tenant_key", "postgres_password", "redis_password", "stub_token")}
        data["approval_key"] = base64.b64encode(secrets.token_bytes(32)).decode()
        write(out, dump(data))
    else:
        secret_path = Path(args.secrets)
        if secret_path.stat().st_mode & 0o077:
            raise ValueError("secret file must be private (0600)")
        render(topology(args.config), json.loads(secret_path.read_text()), out)
    print("Private output created; do not commit or print its contents.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError):
        raise SystemExit("Invalid input or output; existing files were preserved.")
