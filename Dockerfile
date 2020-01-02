# ffmpeg image
FROM jrottenberg/ffmpeg:4.0-alpine AS ffmpeg

# golang image
FROM golang:1.11.5-alpine3.8 AS build

# copy the source files
COPY . /go/src/github.com/webtor-io/content-transcoder

# set workdir
WORKDIR /go/src/github.com/webtor-io/content-transcoder/server

# enable modules
ENV GO111MODULE=on

# disable crosscompiling 
ENV CGO_ENABLED=0

# compile linux only
ENV GOOS=linux

# build the binary with debug information removed
RUN go build -mod=vendor -ldflags '-w -s' -a -installsuffix cgo -o server

FROM alpine:3.8

# copy static ffmpeg to use later 
COPY --from=ffmpeg /usr/local /usr/local

# install additional dependencies for ffmpeg
RUN apk add --no-cache --update libgcc libstdc++ ca-certificates libcrypto1.0 libssl1.0 libgomp expat

# copy our static linked library
COPY --from=build /go/src/github.com/webtor-io/content-transcoder/server/server .

# make output dir
RUN mkdir ./out

# tell we are exposing our service on port 8080 and 8081
EXPOSE 8080 8081

# run it!
CMD ["./server"]
