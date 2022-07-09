# golang image
FROM golang:latest AS build

# set work dir
WORKDIR /app

# copy the source files
COPY . .

# disable crosscompiling 
ENV CGO_ENABLED=0

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN go build -ldflags '-w -s' -a -installsuffix cgo -o server

FROM jrottenberg/ffmpeg:5.0.1-alpine313 AS ffmpeg

# set work dir
WORKDIR /app

# copy our static linked library
COPY --from=build /app/server .

# copy player
COPY --from=build /app/player ./player

# make output dir
RUN mkdir ./out

# tell we are exposing our service on port 8080 and 8081
EXPOSE 8080 8081

# set default entrypoint
ENTRYPOINT ["./server"]