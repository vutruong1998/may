FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /may ./cmd/may

FROM scratch
COPY --from=build /may /may
EXPOSE 4566
HEALTHCHECK --interval=10s --timeout=3s CMD ["/may", "-healthcheck"]
ENTRYPOINT ["/may"]
