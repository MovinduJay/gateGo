FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gatego ./cmd/gatego

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -H -s /sbin/nologin gatego
COPY --from=build /out/gatego /usr/local/bin/gatego
COPY configs/gatego.json /configs/gatego.json
USER gatego
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gatego", "-config", "/configs/gatego.json"]
