# Runtime image for transpiled mro2nf pipelines, at /opt/mro2nf on PATH, where
# the generated project's tasks find mre: the `mre` shim, the `mrjob` beside it
# that py and comp stages run under, and a copy of the Martian adapters mre
# embeds, with their LICENSE and THIRD_PARTY_NOTICES. A pipeline's image builds
# FROM it and adds the pipeline's code (README: Container image).
#
# Use the tag matching the mro2nf version that generated your project so the mre
# ABI matches. Published multi-arch (linux/amd64, linux/arm64) on each release.

# Build mre and mrjob on the native build platform and cross-compile to the
# target arch, so only the small final stage is emulated when building arm64.
# The image's Go is the go.mod toolchain: the image sets GOTOOLCHAIN=local, so
# it never switches.
#
# Base images are pinned by digest, so a moved tag cannot change the image;
# Dependabot (docker) bumps tag and digest together.
FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS build
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
# mrjob is the pinned Martian module's, as `make build` builds it.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -o /out/mrjob github.com/martian-lang/martian/cmd/mrjob

FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
# bash launcher + ps (Nextflow task metrics) + coreutils + aws CLI (S3 staging on
# AWS Batch). No ENTRYPOINT: the backend invokes the generated command directly.
RUN apt-get update \
 && apt-get install -y --no-install-recommends procps coreutils awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/mre /out/mrjob /opt/mro2nf/
COPY vendor-martian/python /opt/mro2nf/adapters/python
COPY LICENSE THIRD_PARTY_NOTICES /opt/mro2nf/
RUN chmod +x /opt/mro2nf/mre /opt/mro2nf/mrjob
ENV PATH="/opt/mro2nf:${PATH}"
