# syntax=docker/dockerfile:1.7

FROM golang:1.26.5 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.buildCommit=${COMMIT} -X main.buildDate=${BUILD_DATE}" -o /out/viper ./cmd/viper

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/viper /usr/local/bin/viper
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/viper"]
