# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/lezard .
RUN apk add --no-cache ca-certificates && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/lezard /lezard
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV LEZARD_DATA=/data LEZARD_LISTEN=:3070
EXPOSE 3070
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/lezard", "-healthcheck"]
ENTRYPOINT ["/lezard"]
