# Этап 1: Сборка
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o chronos-lock ./cmd

# Этап 2: Минимальный рантайм
FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/chronos-lock .
EXPOSE 8080
CMD ["./chronos-lock"]
