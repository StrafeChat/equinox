# Dockerfile for equinox
FROM golang:1.23

WORKDIR /app

COPY . .

RUN go mod tidy && \
    CGO_ENABLED=0 go build -o equinox ./src/main.go

EXPOSE 4000

CMD ["./equinox"]