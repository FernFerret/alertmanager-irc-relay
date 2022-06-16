FROM golang:1.16 as build

WORKDIR /go/src/app
COPY . .

RUN CGO_ENABLED=0 go build -o /tmp/alertmanager-irc-relay -v .

FROM scratch

COPY --from=build /tmp/alertmanager-irc-relay /usr/local/bin/alertmanager-irc-relay

CMD ["alertmanager-irc-relay", "--config=/config.yml"]
