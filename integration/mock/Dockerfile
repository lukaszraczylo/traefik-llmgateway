# Build context is integration/ (see docker-compose.yml's build.context),
# so this Dockerfile can reach both go.mod and the mock/ package.
FROM golang:1.22-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod ./
COPY mock ./mock
RUN go build -o /out/mock ./mock

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates
COPY --from=build /out/mock /usr/local/bin/mock
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/mock"]
CMD ["-mode", "openai"]
