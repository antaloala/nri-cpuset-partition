# Build a static binary: the runtime image has no libc and the plugin only
# ever talks to the NRI socket and to cgroupfs.
FROM golang:1.27 AS build

ARG VERSION=devel

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/nri-cpuset-partition ./cmd

# registry.suse.com/bci/bci-micro:16 is the drop-in alternative if the images
# have to come from the SUSE registry.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nri-cpuset-partition /usr/local/bin/nri-cpuset-partition

# The plugin writes to cgroupfs, so it runs as root despite the nonroot base.
USER 0:0
ENTRYPOINT ["/usr/local/bin/nri-cpuset-partition"]
