# Siphon's server mode in a container: the download queue, driven from a
# browser at http://<host>:8080.
#
#   docker build -t siphon .
#   docker run -d -p 8080:8080 -v ./downloads:/downloads -v ./siphon-data:/data siphon
#   docker logs <container>        # prints the address with the token
#
# /downloads is where files go; /data keeps the queue and the token, so
# unfinished downloads go on after a restart. Set SIPHON_TOKEN to choose the
# token yourself.

# The build stage runs on the builder's own platform and cross-compiles, so
# an arm64 image doesn't build Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# The command-line program needs no cgo: a static binary.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/siphon .

FROM alpine:3.22
# ca-certificates for HTTPS; 7zip unpacks 7z, rar and archives in parts
# (-extract); tzdata so download hours and times follow TZ.
RUN apk add --no-cache ca-certificates 7zip tzdata \
 && adduser -D -H -u 1000 siphon \
 && mkdir -p /downloads /data \
 && chown siphon:siphon /downloads /data
COPY --from=build /out/siphon /usr/local/bin/siphon
USER siphon
VOLUME ["/downloads", "/data"]
EXPOSE 8080
ENTRYPOINT ["siphon", "serve", "-addr", ":8080", "-out", "/downloads", "-data", "/data"]
