FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pi-agent .

FROM alpine:3.22
RUN addgroup -S pi && adduser -S -G pi pi \
    && mkdir -p /var/lib/pi-agent/audit \
    && chown -R pi:pi /var/lib/pi-agent
USER pi
WORKDIR /app
COPY --from=build /out/pi-agent /usr/local/bin/pi-agent
ENTRYPOINT ["pi-agent"]
CMD ["help"]
