FROM golang:alpine as builder
ARG LDFLAGS=""

RUN apk --update --no-cache add git build-base gcc

COPY . /build
WORKDIR /build

# Build both the NetFlow collector and the aggregator
RUN go build -ldflags "${LDFLAGS}" -o goflow2 cmd/goflow2/main.go
RUN go build -o aggregator cmd/aggregator/main.go

FROM alpine:latest
ARG src_dir
ARG VERSION=""
ARG CREATED=""
ARG DESCRIPTION=""
ARG NAME=""
ARG MAINTAINER=""
ARG URL=""
ARG LICENSE=""
ARG REV=""

LABEL org.opencontainers.image.created="${CREATED}"
LABEL org.opencontainers.image.authors="${MAINTAINER}"
LABEL org.opencontainers.image.url="${URL}"
LABEL org.opencontainers.image.title="${NAME}"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.description="${DESCRIPTION}"
LABEL org.opencontainers.image.licenses="${LICENSE}"
LABEL org.opencontainers.image.revision="${REV}"

RUN apk update --no-cache && \
    adduser -S -D -H -h / flow && \
    apk add --no-cache supervisor

# Copy binaries from builder
COPY --from=builder /build/goflow2 /
COPY --from=builder /build/aggregator /

# Set up supervisord configuration
RUN mkdir -p /etc/supervisor/conf.d
COPY cmd/aggregator/supervisord.conf /etc/supervisor/supervisord.conf

# Create log directory
RUN mkdir -p /var/log

CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf"]
