package cluster

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
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

func decodeBase64Field(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// s3PutBucket creates an S3 bucket using AWS Signature V4 authentication.
func s3PutBucket(ctx context.Context, endpoint, accessKey, secretKey, bucket string) error {
	url := fmt.Sprintf("%s/%s", strings.TrimRight(endpoint, "/"), bucket)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, nil)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzdate := now.Format("20060102T150405Z")
	region := "us-east-1"
	service := "s3"

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", amzdate)
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") // empty body hash

	// Canonical request
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.URL.Host, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", amzdate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("PUT\n/%s\n\n%s\n%s\n%s", bucket, canonicalHeaders, signedHeaders,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	// String to sign
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzdate, credentialScope, hex.EncodeToString(hash[:]))

	// Signing key
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	sigHash := hmacSHA256(kSigning, []byte(stringToSign))
	signature := hex.EncodeToString(sigHash)

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature))

	resp, err := s3HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("S3 PUT bucket: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 409 {
		return nil // bucket already exists
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("S3 PUT bucket returned %d", resp.StatusCode)
	}
	return nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
