FROM golang:1.26-alpine AS build

# go.mod pins the language version; if it moves ahead of this image, fetch the
# toolchain it asks for rather than failing an operator's first build outright.
ENV GOTOOLCHAIN=auto

WORKDIR /app

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -o stargate ./cmd/stargate
RUN CGO_ENABLED=0 GOOS=linux go build -o migrate ./cmd/migrate

FROM alpine:3.20

WORKDIR /app

RUN apk add --no-cache ca-certificates && update-ca-certificates

COPY --from=build /app/api /app/stargate /app/migrate /app/
COPY --from=build /app/migrations /app/migrations

# Default command runs the HTTP API; override to "./stargate" for Stargate or
# "./migrate -create-keyspace" to apply pending schema migrations.
CMD ["./api"]

