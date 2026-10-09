FROM golang:1.27.2-alpine3.22 AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mock-node ./cmd/mock-node

FROM alpine:3.22.2 AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /out/gateway /usr/local/bin/cordova-gateway
COPY --from=build /out/mock-node /usr/local/bin/cordova-mock-node
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cordova-gateway"]

FROM runtime AS gateway
EXPOSE 8080

FROM runtime AS mock-node
ENTRYPOINT ["/usr/local/bin/cordova-mock-node"]
EXPOSE 8090
