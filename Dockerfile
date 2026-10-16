# idxa container image.
#
# The parser links libpg_query, so the build needs cgo. Building against musl
# and linking statically means the finished image needs no libc at all, which
# is why the runtime stage can be this small.

FROM golang:1.26-alpine AS build

# musl-dev and gcc are the cgo toolchain; git is for the version stamp.
RUN apk add --no-cache gcc musl-dev git

WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build \
      -ldflags "-s -w \
        -linkmode external -extldflags '-static' \
        -X github.com/indexbeacon/indexbeacon/cmd.version=${VERSION} \
        -X github.com/indexbeacon/indexbeacon/cmd.commit=${COMMIT} \
        -X github.com/indexbeacon/indexbeacon/cmd.date=${DATE}" \
      -o /out/idxa . \
 && /out/idxa version

# The runtime image carries the binary, certificates for TLS connections to
# managed databases, and timezone data, which the scheduler needs to honour a
# cron expression in a named zone.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 65532 idxa \
 && adduser -S -u 65532 -G idxa -H -D idxa \
 && mkdir -p /var/lib/idxa \
 && chown idxa:idxa /var/lib/idxa

COPY --from=build /out/idxa /usr/local/bin/idxa

# Configuration and saved analyses live here. Mount a volume to keep them
# across restarts; see deploy/kubernetes.
ENV XDG_CONFIG_HOME=/var/lib/idxa
VOLUME /var/lib/idxa

USER 65532:65532
EXPOSE 8777

# Listening on all interfaces is required inside a container, since nothing
# outside it could reach a loopback address. That means the access token is
# on, which is what --allow-remote turns on. Supply IDXA_TOKEN to set it
# yourself; otherwise one is generated and printed at startup.
ENTRYPOINT ["idxa"]
CMD ["serve", "--listen", "0.0.0.0:8777", "--allow-remote"]
