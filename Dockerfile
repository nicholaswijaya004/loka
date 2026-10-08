# One image per Loka binary: CMD picks which ./cmd/<name> to build.
#   docker build --build-arg CMD=api -t loka-api .
FROM golang:1.26-alpine AS build
WORKDIR /src
# Dependencies first: this layer is cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=api
# Static binary: the runtime image has no C library to link against.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# Alpine rather than distroless: its busybox wget is the compose healthcheck.
FROM alpine:3.22
RUN adduser -D -u 10001 app
COPY --from=build /out/app /usr/local/bin/app
USER app
ENTRYPOINT ["/usr/local/bin/app"]
