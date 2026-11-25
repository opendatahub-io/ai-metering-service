FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /metering-service ./cmd/metering-service

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.5

COPY --from=builder /metering-service /metering-service

EXPOSE 8080

ENTRYPOINT ["/metering-service"]
