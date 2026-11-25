package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// This package issues its own SeaweedFS IAM and S3 requests, each signed with
// AWS Signature Version 4. Embedded IAM is exposed on the same port as S3 when
// "weed mini" runs ("weed/command/mini.go" "s3.iam", default true at 4.54): POST
// / with a form-encoded Action, matching what the AWS IAM query API expects.
// Traffic targets the storage Service directly—not the Kubernetes API—so no
// extra RBAC entries apply (see SECURITY.md).

// emptyPayloadHash holds the SHA-256 digest for a zero-length payload.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// awsEscape applies the URI encoding Signature Version 4 requires: bytes outside
// A-Z, a-z, 0-9, '-', '.', '_', and '~' become %XX in upper case; '/' as well
// unless keepSlash is set.
func awsEscape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9', ch == '-', ch == '.', ch == '_', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && keepSlash:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// canonicalQuery builds a Signature Version 4 query string: keys sorted, then
// values, with URI encoding on each component.
func canonicalQuery(q url.Values) string {
	// Sorted by encoded key, then encoded value (not by the joined "k=v"
	// text, where "a-b=" would sort before "a=").
	type pair struct{ k, v string }
	var pairs []pair
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, pair{awsEscape(k, false), awsEscape(v, false)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// signV4 produces an Authorization header for req (canonical path, RawQuery from
// canonicalQuery) using service and S3Region.
func signV4(req *http.Request, payload []byte, accessKey, secretKey, service string, now time.Time) {
	payloadHash := emptyPayloadHash
	if len(payload) > 0 {
		sum := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(sum[:])
	}
	datestamp := now.UTC().Format("20060102")
	amzdate := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzdate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", req.URL.Host, payloadHash, amzdate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{req.Method, path, req.URL.RawQuery, canonicalHeaders, signedHeaders, payloadHash}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, S3Region, service)
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzdate, credentialScope, hex.EncodeToString(hash[:]))
	kDate := HMACSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
	kRegion := HMACSHA256(kDate, []byte(S3Region))
	kService := HMACSHA256(kRegion, []byte(service))
	kSigning := HMACSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(HMACSHA256(kSigning, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, credentialScope, signedHeaders, signature))
}

// seaweedClient performs IAM and S3 operations against storage with a single credential pair.
type seaweedClient struct {
	ctx      context.Context
	endpoint string
	keys     S3Keys
	// http is s3HTTPClient, or s3SlowHTTPClient for the key check.
	http *http.Client
}

// s3SlowHTTPClient waits longer for an answer: after "weed mini" restarts
// (Secure S3 storage's rollout), its first object write takes about 11
// seconds while the master learns the volume server's new address (seen
// with 4.54 in Podman), longer than s3HTTPClient's 10 seconds.
var s3SlowHTTPClient = &http.Client{Timeout: 45 * time.Second, Transport: s3HTTPClient.Transport}

func newSeaweedClient(ctx context.Context, endpoint string, keys S3Keys) *seaweedClient {
	return &seaweedClient{ctx: ctx, endpoint: strings.TrimRight(endpoint, "/"), keys: keys, http: s3HTTPClient}
}

// IAMError represents a failed IAM API call (or S3, when the request was S3).
type IAMError struct {
	Status  int
	Code    string
	Message string
}

func (e *IAMError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
}

// iamErrorCode extracts the Code from *IAMError, or "" when err is not one.
func iamErrorCode(err error) string {
	if e, ok := errors.AsType[*IAMError](err); ok {
		return e.Code
	}
	return ""
}

// parseServiceError unmarshals either an IAM ErrorResponse or an S3 Error payload.
func parseServiceError(status int, body []byte) *IAMError {
	var doc struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
		Error   struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	_ = xml.Unmarshal(body, &doc)
	e := &IAMError{Status: status, Code: doc.Error.Code, Message: doc.Error.Message}
	if e.Code == "" {
		e.Code, e.Message = doc.Code, doc.Message
	}
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	return e
}

// send signs and dispatches a single HTTP call, returning status and body. Response
// bodies are capped at 1 MiB because IAM and probe payloads stay small.
func (s *seaweedClient) send(method, path string, query url.Values, contentType string, body []byte, service string) (int, []byte, error) {
	u := s.endpoint + path
	if len(query) > 0 {
		u += "?" + canonicalQuery(query)
	}
	req, err := http.NewRequestWithContext(s.ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body == nil {
		req.Body = http.NoBody
	}
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	signV4(req, body, s.keys.AccessKey, s.keys.SecretKey, service, time.Now())
	kube.NoteRequest(req)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, s.endpoint, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, respBody, err
}

// iam executes one IAM action and yields the raw XML response.
func (s *seaweedClient) iam(action string, params url.Values) ([]byte, error) {
	form := url.Values{"Action": {action}, "Version": {"2010-05-08"}}
	for k, vs := range params {
		form[k] = vs
	}
	body := []byte(canonicalQuery(form))
	status, resp, err := s.send(http.MethodPost, "/", nil, "application/x-www-form-urlencoded; charset=utf-8", body, "iam")
	if err != nil {
		return nil, fmt.Errorf("IAM %s: %w", action, err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("IAM %s: %w", action, parseServiceError(status, resp))
	}
	return resp, nil
}

// CreateUser registers an IAM user; EntityAlreadyExists is treated as success.
func (s *seaweedClient) CreateUser(name string) error {
	_, err := s.iam("CreateUser", url.Values{"UserName": {name}})
	if iamErrorCode(err) == "EntityAlreadyExists" {
		return nil
	}
	return err
}

// DeleteUser removes an IAM user along with inline policies and tags (SeaweedFS
// blocks removal while access keys remain); NoSuchEntity is treated as success.
func (s *seaweedClient) DeleteUser(name string) error {
	_, err := s.iam("DeleteUser", url.Values{"UserName": {name}})
	if iamErrorCode(err) == "NoSuchEntity" {
		return nil
	}
	return err
}

// ListUsers yields all IAM user names.
func (s *seaweedClient) ListUsers() ([]string, error) {
	body, err := s.iam("ListUsers", nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Users []string `xml:"ListUsersResult>Users>member>UserName"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("IAM ListUsers: parse the response: %w", err)
	}
	return doc.Users, nil
}

// CreateAccessKey issues a new access key for user.
func (s *seaweedClient) CreateAccessKey(user string) (S3Keys, error) {
	body, err := s.iam("CreateAccessKey", url.Values{"UserName": {user}})
	if err != nil {
		return S3Keys{}, err
	}
	var doc struct {
		ID     string `xml:"CreateAccessKeyResult>AccessKey>AccessKeyId"`
		Secret string `xml:"CreateAccessKeyResult>AccessKey>SecretAccessKey"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil || doc.ID == "" || doc.Secret == "" {
		return S3Keys{}, fmt.Errorf("IAM CreateAccessKey: the response has no access key")
	}
	return S3Keys{AccessKey: doc.ID, SecretKey: doc.Secret}, nil
}

// ListAccessKeys lists access key IDs for user. found is false when that user
// is absent.
func (s *seaweedClient) ListAccessKeys(user string) (ids []string, found bool, err error) {
	body, err := s.iam("ListAccessKeys", url.Values{"UserName": {user}})
	if iamErrorCode(err) == "NoSuchEntity" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc struct {
		IDs []string `xml:"ListAccessKeysResult>AccessKeyMetadata>member>AccessKeyId"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, false, fmt.Errorf("IAM ListAccessKeys: parse the response: %w", err)
	}
	return doc.IDs, true, nil
}

// DeleteAccessKey revokes one access key belonging to user; a missing key is ignored.
func (s *seaweedClient) DeleteAccessKey(user, id string) error {
	_, err := s.iam("DeleteAccessKey", url.Values{"UserName": {user}, "AccessKeyId": {id}})
	if iamErrorCode(err) == "NoSuchEntity" {
		return nil
	}
	return err
}

// PutUserPolicy attaches an inline policy document under name on user.
func (s *seaweedClient) PutUserPolicy(user, name, document string) error {
	_, err := s.iam("PutUserPolicy", url.Values{"UserName": {user}, "PolicyName": {name}, "PolicyDocument": {document}})
	return err
}

// ListUserTags returns user's tags. found is false when the user does not exist.
func (s *seaweedClient) ListUserTags(user string) (tags map[string]string, found bool, err error) {
	body, err := s.iam("ListUserTags", url.Values{"UserName": {user}})
	if iamErrorCode(err) == "NoSuchEntity" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"ListUserTagsResult>Tags>member"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, false, fmt.Errorf("IAM ListUserTags: parse the response: %w", err)
	}
	tags = map[string]string{}
	for _, t := range doc.Tags {
		tags[t.Key] = t.Value
	}
	return tags, true, nil
}

// TagUser applies a single key/value tag to user.
func (s *seaweedClient) TagUser(user, key, value string) error {
	_, err := s.iam("TagUser", url.Values{"UserName": {user}, "Tags.member.1.Key": {key}, "Tags.member.1.Value": {value}})
	return err
}

// object issues a path-style S3 request for key within bucket; key "" addresses
// the bucket root.
func (s *seaweedClient) object(method, bucket, key string, query url.Values, body []byte) (int, []byte, error) {
	path := "/" + bucket
	if key != "" {
		path += "/" + awsEscape(key, true)
	}
	return s.send(method, path, query, "", body, "s3")
}
