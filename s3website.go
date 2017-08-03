package s3website

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

var (
	base64Encoding           = base64.StdEncoding.WithPadding(base64.NoPadding)
	compressableContentTypes = map[string]bool{
		"application/eot":               true,
		"application/font":              true,
		"application/font-sfnt":         true,
		"application/javascript":        true,
		"application/json":              true,
		"application/opentype":          true,
		"application/otf":               true,
		"application/pkcs7-mime":        true,
		"application/truetype":          true,
		"application/ttf":               true,
		"application/vnd.ms-fontobject": true,
		"application/x-font-opentype":   true,
		"application/x-font-truetype":   true,
		"application/x-font-ttf":        true,
		"application/x-httpd-cgi":       true,
		"application/x-javascript":      true,
		"application/x-mpegurl":         true,
		"application/x-opentype":        true,
		"application/x-otf":             true,
		"application/x-perl":            true,
		"application/x-ttf":             true,
		"application/xhtml+xml":         true,
		"application/xml":               true,
		"application/xml+rss":           true,
		"font/eot":                      true,
		"font/opentype":                 true,
		"font/otf":                      true,
		"font/ttf":                      true,
		"image/svg+xml":                 true,
		"text/css":                      true,
		"text/csv":                      true,
		"text/html":                     true,
		"text/javascript":               true,
		"text/js":                       true,
		"text/plain":                    true,
		"text/richtext":                 true,
		"text/tab-separated-values":     true,
		"text/x-component":              true,
		"text/x-java-source":            true,
		"text/x-script":                 true,
		"text/xml":                      true,
	}
)

func acceptEncodingGzip(req *http.Request) bool {
	encodings := strings.Split(req.Header.Get("accept-encoding"), ",")
	for _, encoding := range encodings {
		if strings.TrimSpace(encoding) == "gzip" {
			return true
		}
	}

	return false
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

type S3Website struct {
	client *s3.S3
	bucket *string
}

func NewS3Website(client *s3.S3, bucket string) *S3Website {
	return &S3Website{
		client: client,
		bucket: aws.String(bucket),
	}
}

func (s *S3Website) headObject(key string) (*s3.HeadObjectOutput, error) {
	headObjectInput := &s3.HeadObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(key),
	}

	headObjectOutput, err := s.client.HeadObject(headObjectInput)
	if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == "NotFound" {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return headObjectOutput, nil
}

func (s *S3Website) getObject(key string) (*s3.GetObjectOutput, error) {
	getObjectInput := &s3.GetObjectInput{
		Bucket: s.bucket,
		Key:    aws.String(key),
	}

	getObjectOutput, err := s.client.GetObject(getObjectInput)
	if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == "NoSuchKey" {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return getObjectOutput, nil
}

func (s *S3Website) serveFile(w http.ResponseWriter, req *http.Request, key string) {
	getObjectOutput, err := s.getObject(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if getObjectOutput == nil {
		http.NotFound(w, req)
		return
	}

	// Get the file content-type from the S3 output.
	var fileContentType string
	if getObjectOutput.ContentType != nil {
		fileContentType = *getObjectOutput.ContentType
	}

	// If the content-type wasn't set in the S3 output, try to guess it based on
	// the file extension.
	if fileContentType == "" {
		fileContentType = mime.TypeByExtension(filepath.Ext(key))
	}

	// Set the response Content-Type header.
	if fileContentType != "" {
		w.Header().Set("content-type", fileContentType)
	}

	var data *bytes.Buffer
	if getObjectOutput.ContentLength != nil {
		data = bytes.NewBuffer(make([]byte, 0, *getObjectOutput.ContentLength))
	} else {
		data = bytes.NewBuffer([]byte{})
	}

	var dataWriteCloser io.WriteCloser

	gzipEncoded := false
	if compressableContentTypes[strings.Split(fileContentType, ";")[0]] && acceptEncodingGzip(req) {
		gzipEncoded = true
		dataWriteCloser = gzip.NewWriter(data)
	} else {
		dataWriteCloser = nopWriteCloser{data}
	}

	fileHash := sha256.New()
	dataMultiWriter := io.MultiWriter(dataWriteCloser, fileHash)
	if _, err := io.Copy(dataMultiWriter, getObjectOutput.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := dataWriteCloser.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set an ETag header based on the SHA256 hash.
	fileHashSum := fileHash.Sum(nil)
	etag := `"` + base64Encoding.EncodeToString(fileHashSum) + `"`
	w.Header().Set("etag", etag)

	// If the file is gzip-encoded, set a Content-Encoding header.
	if gzipEncoded {
		w.Header().Set("content-encoding", "gzip")
		w.Header().Set("vary", "accept-encoding")
	}

	// Set a Content-Type header.
	w.Header().Set("content-type", fileContentType)

	// Set a Cache-Control header.
	if cacheControl := aws.StringValue(getObjectOutput.CacheControl); cacheControl != "" {
		w.Header().Set("cache-control", aws.StringValue(getObjectOutput.ContentType))
	} else {
		w.Header().Set("cache-control", "max-age=60")
	}

	http.ServeContent(w, req, key, aws.TimeValue(getObjectOutput.LastModified), bytes.NewReader(data.Bytes()))
}

func (s *S3Website) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key := req.URL.Path
	if strings.HasSuffix(key, "/") {
		s.serveFile(w, req, key+"index.html")
		return
	}

	headObjectOutput, err := s.headObject(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if headObjectOutput != nil {
		s.serveFile(w, req, key)
		return
	}

	headObjectOutput, err = s.headObject(key + "/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if headObjectOutput != nil {
		http.Redirect(w, req, key+"/", 302)
		return
	}

	http.NotFound(w, req)
}
