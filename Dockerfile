ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./ 
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/heimdall ./cmd/heimdall

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -g '' heimdall
USER heimdall
COPY --from=build /bin/heimdall /usr/local/bin/heimdall
EXPOSE 8080 9090
ENTRYPOINT ["heimdall"]
