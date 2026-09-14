FROM golang:1.25.10-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /canary-shop .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 shop && adduser -D -u 10001 -G shop shop
COPY --from=build /canary-shop /usr/local/bin/canary-shop
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["canary-shop"]
CMD ["serve"]
