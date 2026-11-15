# syntax=docker/dockerfile:1
#
# Targets:
#   worker (default)  whisper-network + whisper-cli + ffmpeg. Can also run `server`.
#   server            whisper-network only, on distroless (no media tools at all).

# Base images are pinned by digest for reproducible builds; dependabot's docker
# ecosystem updates the tag and digest together.
# GO_VERSION is informational; the golang base image tag below is what is used.
ARG GO_VERSION=1.27.4
ARG WHISPER_VERSION=v1.9.5
# Commit that WHISPER_VERSION must resolve to (guards against a moved tag).
ARG WHISPER_COMMIT=d1be6fde11ac6e0407606b4e42fe72d34add8037

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS debian
# The Go toolchain runs on the build host's platform and cross-compiles, so
# multi-arch builds never emulate the Go build.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS golang
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS distroless

# ---------------------------------------------------------------------------
# whisper.cpp: pinned release, portable CPU build.
FROM debian AS whisper
ARG WHISPER_VERSION
ARG WHISPER_COMMIT
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${WHISPER_VERSION}" https://github.com/ggml-org/whisper.cpp /src
WORKDIR /src
RUN test "$(git rev-parse HEAD)" = "${WHISPER_COMMIT}"
# On amd64, ggml builds every CPU variant (SSE4.2 … AVX-512) as loadable
# backends and selects the best one at runtime. Other architectures get a
# portable baseline build.
RUN if [ "${TARGETARCH}" = "amd64" ]; then \
      VARIANTS="-DGGML_BACKEND_DL=ON -DGGML_CPU_ALL_VARIANTS=ON"; \
    else \
      VARIANTS=""; \
    fi \
 && cmake -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=ON \
      -DGGML_NATIVE=OFF ${VARIANTS} \
      -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_SERVER=OFF \
 && cmake --build build -j"$(nproc)" --target whisper-cli \
 && mkdir -p /opt/whisper \
 && cp -a build/bin/whisper-cli build/bin/*.so* /opt/whisper/

# ---------------------------------------------------------------------------
# whisper-network: pure Go, static.
FROM golang AS builder
ARG VERSION=development
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X 'github.com/deweysasser/whisper-network/program.Version=${VERSION}'" \
      -o /out/whisper-network . \
 && install -d -m 0700 /out/data

# ---------------------------------------------------------------------------
FROM distroless AS server
COPY --from=builder /out/whisper-network /usr/local/bin/whisper-network
# Mode 0700 (set explicitly: COPY would otherwise create the destination 0755),
# owned by the non-root user. A new named volume copies this
# directory's ownership and mode, so the non-root server
# can write to it without a chown step.
COPY --from=builder --chown=65532:65532 --chmod=0700 /out/data /data
VOLUME ["/data"]
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/whisper-network"]
CMD ["server"]

# ---------------------------------------------------------------------------
FROM debian AS worker
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /var/lib/whisper-network --create-home whisper
# whisper-cli and its libraries live together in /opt/whisper: ggml loads the
# CPU backend variants from the executable's directory, and the worker's
# sandbox grants read access to that directory.
COPY --from=whisper /opt/whisper/ /opt/whisper/
ENV PATH=/opt/whisper:$PATH
RUN echo /opt/whisper > /etc/ld.so.conf.d/whisper.conf && ldconfig \
 && whisper-cli --version && ffmpeg -version | head -1
COPY --from=builder /out/whisper-network /usr/local/bin/whisper-network
USER 10001
VOLUME ["/var/lib/whisper-network"]
ENTRYPOINT ["/usr/local/bin/whisper-network"]
CMD ["worker", "--help"]
