# Runtime base image for transpiled mro2nf pipelines: the `mre` shim, the
# Martian Python adapters, and the -native-runner Python runner at the same
# /opt/mro2nf paths the transpiler bakes into its generated scripts. Build your
# pipeline image FROM this and add stage code:
#
#   FROM ghcr.io/eunmann/mro2nf-runtime:<version>
#   COPY stages /opt/mro2nf/stages
#
# Use the tag matching the mro2nf version that generated your project so the mre
# ABI matches. Published multi-arch (linux/amd64, linux/arm64) on each release.
# `comp`-adapter stages also need an mrjob binary at /opt/mro2nf/mrjob — add it in
# your downstream image (this base covers the common py/exec stages).

# Build mre on the native build platform and cross-compile to the target arch, so
# only the small final stage is emulated when building arm64.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# VERSION is the release tag baked into `mre version` (matches the Makefile's
# -X main.version ldflag); release.yml passes it via --build-arg.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/mre ./cmd/mre

FROM python:3.12-slim
# bash launcher + ps (Nextflow task metrics) + coreutils + aws CLI (S3 staging on
# AWS Batch). No ENTRYPOINT: the backend invokes the generated command directly.
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps coreutils awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/mre /opt/mro2nf/mre
COPY vendor-martian/python /opt/mro2nf/adapters
# The -native-runner runner at the fixed path generated container projects
# reference. Named files (not the dir) so test_runner.py never ships, mirroring
# the transpiler's embed (internal/emit/runnerassets.go).
COPY internal/emit/runner/run_stage.py internal/emit/runner/martian.py /opt/mro2nf/runner/
RUN chmod +x /opt/mro2nf/mre
