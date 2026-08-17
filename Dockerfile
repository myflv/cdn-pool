FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /cdn-pool .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /cdn-pool /usr/local/bin/cdn-pool
COPY config.yaml /app/config.yaml
COPY ip.txt /app/ip.txt
EXPOSE 1080
ENTRYPOINT ["cdn-pool", "-c", "/app/config.yaml"]
