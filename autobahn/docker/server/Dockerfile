FROM golang:1.20.2-alpine3.17

WORKDIR /go/src/github.com/dinhngocdiep/ws

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
ENV CGO_ENABLED=0
RUN go test -c -tags autobahn -coverpkg "github.com/dinhngocdiep/ws/..." github.com/dinhngocdiep/ws/example/autobahn

CMD ["./autobahn.test", "-test.coverprofile", "/report/server.coverage"]
