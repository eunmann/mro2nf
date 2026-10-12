# Runtime base image for transpiled mro2nf pipelines: the `mre` shim and the
# Martian Python adapters at the same /opt/mro2nf paths the transpiler bakes
# into its generated scripts, with their LICENSE and THIRD_PARTY_NOTICES. Build your
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
# only the small final stage is emulated when building arm64. The image's Go is
# the go.mod toolchain: the image sets GOTOOLCHAIN=local, so it never switches.
#
# Base images are pinned by digest, so a moved tag cannot change the image;
# Dependabot (docker) bumps tag and digest together. The docker isolation
# suite runs on the same final-stage base (test/e2e/docker_test.go
# runtimeBase).
FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# VERSION is the release tag baked into `mre version` (matches the Makefile's
# -X main.version ldflag); release.yml passes it via --build-arg.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mre ./cmd/mre

FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
# bash launcher + ps (Nextflow task metrics) + coreutils + aws CLI (S3 staging on
# AWS Batch). No ENTRYPOINT: the backend invokes the generated command directly.
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps coreutils awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/mre /opt/mro2nf/mre
COPY vendor-martian/python /opt/mro2nf/adapters
COPY LICENSE THIRD_PARTY_NOTICES /opt/mro2nf/
RUN chmod +x /opt/mro2nf/mre
