# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/uatu ./cmd/uatu \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/uatu /uatu
# Pre-create /data owned by the nonroot user so fresh volumes are writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV UATU_DB_PATH=/data/uatu.db
VOLUME ["/data"]
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/uatu"]
CMD ["serve"]
