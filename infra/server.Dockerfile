FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mock-node ./cmd/mock-node \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ranker ./cmd/ranker

FROM alpine:3.24.2 AS runtime
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/gateway /usr/local/bin/cordova-gateway
COPY --from=build /out/mock-node /usr/local/bin/cordova-mock-node
COPY --from=build /out/ranker /usr/local/bin/cordova-ranker
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cordova-gateway"]

FROM runtime AS gateway
EXPOSE 8080

FROM runtime AS mock-node
ENTRYPOINT ["/usr/local/bin/cordova-mock-node"]
EXPOSE 8090

FROM runtime AS ranker
ENTRYPOINT ["/usr/local/bin/cordova-ranker"]
