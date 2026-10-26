# Runtime image for transpiled mro2nf pipelines, at /opt/mro2nf on PATH, where
# the generated project's tasks find mre: the `mre` shim, the `mrjob` beside it
# that py and comp stages run under, and a copy of the Martian adapters mre
# embeds, with their LICENSE and THIRD_PARTY_NOTICES. A pipeline's image builds
# FROM it and adds the pipeline's code (docs/usage.md, "Container images").
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
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
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
# Versions are not pinned: Debian drops a superseded version from its mirrors
# at each point and security release, so a pin fails the build, and
# snapshot.debian.org pins would freeze the security fixes. The upgrade takes
# the fixes released since the base image was built. A release pushes the
# image it built, by digest.
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends procps coreutils awscli \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/mre /out/mrjob /opt/mro2nf/
COPY vendor-martian/python /opt/mro2nf/adapters/python
COPY LICENSE THIRD_PARTY_NOTICES /opt/mro2nf/
ENV PATH="/opt/mro2nf:${PATH}"
