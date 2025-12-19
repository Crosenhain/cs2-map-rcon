FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o cs2mapserver

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/cs2mapserver .
EXPOSE 16969
ENTRYPOINT ["./cs2mapserver"]
