# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o server .

# Runtime stage
FROM alpine:3.19

RUN apk add --no-cache ffmpeg ca-certificates

WORKDIR /app

COPY --from=builder /app/server .
COPY templates/ ./templates/
COPY static/ ./static/

# Create data directories
RUN mkdir -p /app/data /app/videos /app/thumbnails

EXPOSE 8080

CMD ["./server"]
