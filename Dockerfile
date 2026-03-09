FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /lokilens ./cmd/lokilens

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /lokilens /lokilens

EXPOSE 8080
CMD ["/lokilens"]
