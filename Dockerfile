# Multi-arch build (linux/amd64, linux/arm64 — see docs/DECISIONS.md:
# "arm64 is required, not optional. Synology ARM models, Raspberry Pis
# and Apple Silicon dev machines are all arm64.") Build with:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t framewright .
#
# Stage 1 compiles a static Go binary for whichever platform buildx is
# targeting (TARGETOS/TARGETARCH are set automatically). Stage 2 is the
# actual runtime image — ffmpeg installed from Debian's own repos, NOT
# bundled into the Go binary (CLAUDE.md: redistributing ffmpeg builds
# carries GPL/LGPL obligations that vary by build configuration —
# avoidable entirely by not shipping it).

FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/framewright ./cmd/framewright

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -u 1000 -m -s /usr/sbin/nologin framewright
COPY --from=build /out/framewright /usr/local/bin/framewright

ENV DATA_DIR=/data
ENV PORT=8420
VOLUME /data
RUN mkdir -p /data && chown framewright:framewright /data
USER framewright

EXPOSE 8420
ENTRYPOINT ["/usr/local/bin/framewright"]
