FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/selfaction .

FROM alpine:3.22
# docker-cli: agent --mode docker drives the host daemon through a mounted socket.
RUN apk add --no-cache ca-certificates docker-cli git
COPY --from=build /out/selfaction /usr/local/bin/selfaction
ENV SELFACTION_HOME=/data
VOLUME /data
EXPOSE 7979
ENTRYPOINT ["selfaction"]
CMD ["cache", "serve"]
