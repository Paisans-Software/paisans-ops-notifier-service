FROM golang:1.25.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /paisans-ops-notifier-service ./cmd/paisans-ops-notifier-service

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /paisans-ops-notifier-service /paisans-ops-notifier-service
EXPOSE 8085
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/paisans-ops-notifier-service", "healthcheck"]
ENTRYPOINT ["/paisans-ops-notifier-service"]
