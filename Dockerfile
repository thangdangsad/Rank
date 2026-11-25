# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bracket . && \
    mkdir -p /out/data

# ---- run: chỉ có binary (web/ đã nhúng), không shell, chạy user nonroot ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bracket /bracket
# /data thuộc user nonroot để volume mới tạo ghi được file SQLite
COPY --from=build --chown=65532:65532 /out/data /data
ENV DB_PATH=/data/bracket.db PORT=8080
EXPOSE 8081
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/bracket", "health"]
ENTRYPOINT ["/bracket"]
