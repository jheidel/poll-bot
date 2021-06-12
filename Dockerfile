# Build binary
FROM golang:alpine AS builder-go
RUN apk add --update make git
WORKDIR /app
COPY . .
RUN make

# Copy to minmal image
FROM alpine
WORKDIR /app
COPY --from=builder-go /app/poll-bot /app
CMD ["./poll-bot"]
