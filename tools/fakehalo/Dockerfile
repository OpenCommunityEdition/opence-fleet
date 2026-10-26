# fakehalo as an image of the server image's shape (the game repository's
# deploy/docker/Dockerfile): the same entrypoint (halo --server), /data
# mounted read-only, /state the only folder written, the user 10001, the
# ports, the environment and STOPSIGNAL. CI runs the container runtime
# against it as nodes run the real image. Built from the repository's root:
#
#   docker build -f tools/fakehalo/Dockerfile -t fakehalo-server .
#   docker run --rm -v "$PWD/maps-dir:/data:ro" -p 127.0.0.1:8080:8080 fakehalo-server
#
# (fakehalo --make-maps <dir> writes maps with valid headers into <dir>/maps)
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux TARGETARCH=amd64
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/opence/halo ./tools/fakehalo \
	&& mkdir -p /out/data /out/state

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/opence /opt/opence
COPY --from=build /out/data /data
COPY --from=build --chown=10001:10001 /out/state /state
ENV HALO_SERVER=1 \
	HALO_CONFIG_FILE=/state/config.toml \
	HALO_DATA_ROOT=/data \
	HALO_SAVE_ROOT=/state/saves \
	HALO_LOG_ROOT=/state/logs \
	HALO_NET_TUNNEL_PORT=2302 \
	HALO_SERVER_WEB_BIND=0.0.0.0 \
	HALO_SERVER_WEB_PORT=8080 \
	HOME=/state
VOLUME ["/state"]
EXPOSE 2302/udp 8080/tcp
USER 10001:10001
WORKDIR /state
HEALTHCHECK --interval=15s --timeout=3s --start-period=60s --retries=3 CMD ["/opt/opence/halo", "--healthz"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/opence/halo", "--server"]
