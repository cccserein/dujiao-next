FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shop ./cmd/shop

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -S shop && adduser -S -G shop shop
COPY --from=build /out/shop /usr/local/bin/shop
USER shop
EXPOSE 8080
ENTRYPOINT ["shop"]
CMD ["serve"]
