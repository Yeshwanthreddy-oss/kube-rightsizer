# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/manager ./cmd/manager

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
