# The control plane as a static binary on a distroless image, run as a
# non-root user. The database, fleet.key, the library's blobs, the releases'
# zips and the updates it installs (bin/) live in /data (a volume). The
# games are not in this image, nor is the Docker socket mounted: a node's
# agent, on the host, makes and owns the servers' containers.
# (the build runs on the builder's platform and cross-compiles: a
# multi-arch image builds without emulating each target)
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux TARGETARCH=amd64 TARGETVARIANT=
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X main.version=${VERSION}" \
	-o /out/opence-fleet ./cmd/opence-fleet \
	&& mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/opence-fleet /usr/local/bin/opence-fleet
COPY --from=build --chown=65532:65532 /out/data /data
ENV FLEET_DATA=/data \
	FLEET_LISTEN=0.0.0.0:8070
VOLUME /data
EXPOSE 8070
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/opence-fleet", "health"]
ENTRYPOINT ["/usr/local/bin/opence-fleet"]
CMD ["serve"]
