# Multi-arch build (linux/amd64, linux/arm64 — see docs/DECISIONS.md:
# "arm64 is required, not optional. Synology ARM models, Raspberry Pis
# and Apple Silicon dev machines are all arm64.") Build with:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t cliphanger .
#
# Stage 1 compiles a static Go binary for whichever platform buildx is
# targeting (TARGETOS/TARGETARCH are set automatically). Stage 2 is the
# actual runtime image — ffmpeg installed from Debian's own repos, NOT
# bundled into the Go binary (CLAUDE.md: redistributing ffmpeg builds
# carries GPL/LGPL obligations that vary by build configuration —
# avoidable entirely by not shipping it).

FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
WORKDIR /src
# go.sum too, not just go.mod (bug fixed 2026-08-25, real deploy
# failure: "missing go.sum entry for module providing package
# golang.org/x/crypto/bcrypt") — this line predates any external
# dependency at all (go.mod alone was enough when the module graph was
# empty), and never got updated once golang.org/x/crypto/bcrypt (for
# local-login password hashing) actually introduced a go.sum to verify
# against. `go build` in `-mod=readonly` mode (the default) has no
# fallback for a missing go.sum — it can't just re-fetch and trust the
# module, so the build fails outright rather than silently skipping
# verification.
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# The `docs` package (docs/docs.go) go:embeds docs/API.md so the web
# UI's Help page can render the real API contract without a hand-copied
# duplicate — internal/web imports it, so the build needs it too. Paired
# with a `!docs/API.md` un-ignore in .dockerignore (the rest of docs/ is
# still excluded from the build context).
COPY docs ./docs

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/cliphanger ./cmd/cliphanger

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -u 1000 -m -s /usr/sbin/nologin cliphanger
COPY --from=build /out/cliphanger /usr/local/bin/cliphanger

ENV DATA_DIR=/data
ENV PORT=8420
VOLUME /data
RUN mkdir -p /data && chown cliphanger:cliphanger /data
USER cliphanger

EXPOSE 8420
ENTRYPOINT ["/usr/local/bin/cliphanger"]
