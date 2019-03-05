FROM golang:alpine as builder

RUN apk --no-cache add git gcc musl-dev binutils

RUN mkdir /build

ADD . /build/

WORKDIR /build

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-extldflags "-static"' -o gitlab-tag . \
    && strip gitlab-tag

FROM alpine

RUN apk --no-cache add ca-certificates libc6-compat

COPY --from=builder /build/gitlab-tag /usr/local/bin

ENTRYPOINT ["gitlab-tag"]
