# Static build so the runtime image can be distroless — an air-gapped install
# mirrors one layer, and there is no shell in the image to attach to.
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/memory-service ./cmd/memory-service

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/memory-service /usr/local/bin/memory-service

USER nonroot:nonroot
EXPOSE 8087

ENTRYPOINT ["/usr/local/bin/memory-service"]
