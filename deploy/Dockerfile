FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/broker ./cmd/broker

FROM alpine:3.22
RUN adduser -D -u 10001 broker && mkdir /data && chown broker /data
COPY --from=build /out/broker /usr/local/bin/broker
USER broker
VOLUME /data
EXPOSE 8080
ENV BROKER_DB=/data/broker.db BROKER_ADDR=:8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["broker"]
CMD ["serve"]
