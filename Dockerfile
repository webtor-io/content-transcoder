# golang image
FROM golang:latest AS build

# set work dir
WORKDIR /app

# copy the source files
COPY . .

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN go build -ldflags '-w -s' -a -installsuffix cgo -o server

FROM jrottenberg/ffmpeg:7-alpine AS ffmpeg

# set work dir
WORKDIR /app

# copy our static linked library
COPY --from=build /app/server .

# copy player
COPY --from=build /app/player ./player

# tell we are exposing our service
EXPOSE 8080 8081 8082

ENTRYPOINT ["./server"]
