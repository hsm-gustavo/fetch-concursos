ARG GO_VERSION=1.26.8
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder
LABEL org.opencontainers.image.source="https://github.com/hsm-gustavo/fetch-concursos"
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /bin/fetch-concursos ./cmd/fetch-concursos

FROM alpine:3.23.3 AS final

LABEL org.opencontainers.image.source="https://github.com/hsm-gustavo/fetch-concursos"

RUN --mount=type=cache,target=/var/cache/apk \
    apk --update add \
        ca-certificates \
        tzdata \
        && \
        update-ca-certificates

ARG UID=10001
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid "${UID}" \
    appuser

WORKDIR /app
RUN mkdir -p /app/data && chown -R appuser:appuser /app

USER appuser

COPY --from=builder /bin/fetch-concursos /app/

ENTRYPOINT [ "/app/fetch-concursos" ]
