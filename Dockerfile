FROM golang:1.23-alpine AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/example/gatego/internal/buildinfo.Version=${VERSION} -X github.com/example/gatego/internal/buildinfo.Commit=${COMMIT} -X github.com/example/gatego/internal/buildinfo.Date=${BUILD_DATE}" -o /out/gatego ./cmd/gatego

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -H -s /sbin/nologin gatego
COPY --from=build /out/gatego /usr/local/bin/gatego
COPY configs/gatego.json /configs/gatego.json
USER gatego
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gatego", "-config", "/configs/gatego.json"]
