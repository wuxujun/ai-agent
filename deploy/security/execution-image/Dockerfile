# Supply a reviewed immutable golang:1.26.8-bookworm repository digest.
ARG BASE_IMAGE=golang@sha256:dc9ad6c05acc7a88e5b71bde60a5fe3bd4b9f0db209011711b464107438a8107
FROM ${BASE_IMAGE}

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates coreutils gcc g++ git make nodejs python3 ripgrep

LABEL ai-agent.execution-image.version="1"
WORKDIR /workspace
USER 65532:65532
