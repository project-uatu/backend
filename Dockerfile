# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/uatu ./cmd/uatu

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/uatu /uatu
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/uatu"]
CMD ["serve"]
