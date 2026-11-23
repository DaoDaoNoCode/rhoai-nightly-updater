package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"time"
)

// s3HTTPClient is a package-level shared HTTP client for S3 operations,
// enabling connection reuse across calls instead of creating a new client
// (and underlying transport/connection pool) on every request.
var s3HTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	},
}

func DecodeBase64Field(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// s3PutBucket creates an S3 bucket using AWS Signature V4 authentication.
func s3PutBucket(ctx context.Context, endpoint, accessKey, secretKey, bucket string) error {
	status, _, err := newSeaweedClient(ctx, endpoint, S3Keys{AccessKey: accessKey, SecretKey: secretKey}).object(http.MethodPut, bucket, "", nil, nil)
	if err != nil {
		return fmt.Errorf("S3 PUT bucket: %w", err)
	}
	if status == http.StatusConflict {
		return nil // bucket already exists
	}
	if status >= 300 {
		return fmt.Errorf("S3 PUT bucket returned %d", status)
	}
	return nil
}

func HMACSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
