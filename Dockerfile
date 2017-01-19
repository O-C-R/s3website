FROM golang:1.7.4-alpine
COPY . $GOPATH/src/github.com/O-C-R/s3website
RUN go install github.com/O-C-R/s3website
CMD ["s3website"]
