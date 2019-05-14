FROM golang:1.11-alpine3.8
RUN apk update && apk add vim tree lsof bash git gcc musl-dev
ENV GOPATH=/home/fonero/go
ENV PATH=/go/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$GOPATH/bin
ENV FNOSRC_PATH=$GOPATH/src/github.com/fonero-project/fnodata/
ENV GO111MODULE=on
RUN adduser -s /bin/bash -D -h /home/fonero fonero && chown -R fonero:fonero /home/fonero
WORKDIR $FNOSRC_PATH
RUN chown -R fonero:fonero $GOPATH 
# since we might be rebulding often we need to cache this module layer
# otherwise docker will detect changes everytime and re-download everything again
COPY go.* $FNOSRC_PATH
RUN go mod download 
COPY . $FNOSRC_PATH
RUN chown -R fonero:fonero $GOPATH 
USER fonero
RUN go build
CMD /bin/bash

ENTRYPOINT ./fnodata
# Note: when building the --squash flag is an experimental feature as of Docker 18.06
# docker build --squash -t fonero-project/fnodata .
# running
# docker run -ti --rm fonero-project/fnodata
# or if attaching source volume and developing interactively
#  docker run -ti --entrypoint=/bin/bash -v ${PWD}:${PWD}:/home/fonero/go/src/github.com/fonero-project/fnodata --rm fonero-project/fnodata