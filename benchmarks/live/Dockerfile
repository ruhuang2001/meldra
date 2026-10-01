FROM golang:1.26.6-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
ENV GOTOOLCHAIN=local GOPROXY=off GOCACHE=/tmp/go-cache \
    PYTHONDONTWRITEBYTECODE=1 HOME=/tmp/home MELDRA_HOME=/tmp/meldra
WORKDIR /workspace
