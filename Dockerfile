FROM alpine:3.16

RUN apk add --update --no-cache ca-certificates curl

ENV DRAINO_VERSION="2.5.3"

RUN curl -Lo ./draino.tar.gz https://github.com/DataDog/draino/releases/download/v${DRAINO_VERSION}/draino_${DRAINO_VERSION}_linux_amd64.tar.gz \
    && tar -xzf draino.tar.gz \
    && rm draino.tar.gz

RUN addgroup -S user && adduser -S user -G user
USER user

ENV PATH="/:${PATH}"