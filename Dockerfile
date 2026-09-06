# The runtime image contains only the static relay binary, an empty state
# directory owned by an unprivileged user, and nothing else: no shell, no
# package manager, no certificates (the relay makes no outbound connections).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /baton-relay . \
 && mkdir -p /state && chown 65534:65534 /state

FROM scratch
COPY --from=build /baton-relay /baton-relay
COPY --from=build --chown=65534:65534 /state /var/lib/baton
USER 65534:65534
EXPOSE 8080
VOLUME ["/var/lib/baton"]
# Drop country CSVs into the volume and add
#   -country-db /var/lib/baton/country-ipv4.csv,/var/lib/baton/country-ipv6.csv
# to the command; see README.md.
ENTRYPOINT ["/baton-relay", "-addr", ":8080", "-state", "/var/lib/baton/state.json"]
