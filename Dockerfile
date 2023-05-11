FROM golang:1.19-alpine3.16 AS build

RUN apk update && apk add git && apk add curl

WORKDIR /go/src/github.com/recoord/draino
COPY . .

RUN go build -o /draino ./cmd/draino

FROM alpine:3.16

RUN apk update && apk add ca-certificates
RUN addgroup -S user && adduser -S user -G user
USER user
COPY --from=build /draino /draino
ENV PATH="/:${PATH}"


#FROM alpine:3.15.6

#ENV LANG en_US.UTF-8
#ENV LANGUAGE en_US:en
#ENV LC_ALL en_US.UTF-8

#USER root
#RUN apk add --update --no-cache ca-certificates
#RUN set -x && update-ca-certificates

#USER nobody
#ADD ./bin /usr/local/bin
