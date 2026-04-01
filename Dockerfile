FROM golang:1.25-alpine AS build

WORKDIR /app

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -o stargate ./cmd/stargate

FROM alpine:3.20

WORKDIR /app

RUN apk add --no-cache ca-certificates && update-ca-certificates

COPY --from=build /app/api /app/stargate /app/

# Default command runs the HTTP API; override to "./stargate" for Stargate.
CMD ["./api"]

