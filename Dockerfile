FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /balancer cmd/balancer/main.go

# ---- Final Stage ----
FROM alpine:latest

WORKDIR /app

COPY --from=builder /balancer /app/balancer
COPY config.yaml /app/config.yaml

EXPOSE 8080

CMD ["/app/balancer"] 