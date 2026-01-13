# golang image
FROM golang:latest AS build

# set work dir
WORKDIR /app

# copy the source files
COPY . .

# compile linux only
ENV GOOS=linux
ENV CGO_ENABLED=0

# build the binary with debug information removed
RUN go build -ldflags '-w -s' -a -installsuffix cgo -o server

FROM jrottenberg/ffmpeg7-alpine AS ffmpeg

# set work dir
WORKDIR /app

# copy our static linked library
COPY --from=build /app/server .

# copy player
COPY --from=build /app/player ./player

ENV OUTPUT=/data

# tell we are exposing our service
EXPOSE 8080 8081 8082 8083

ENTRYPOINT []

# run it!
CMD ["./server"]
