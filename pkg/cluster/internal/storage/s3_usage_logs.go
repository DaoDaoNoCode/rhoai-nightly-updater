package storage

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Usage logs on the S3 storage: RHOAI's monitoring keeps the MaaS usage
// logs in a LokiStack whose object storage is "S3-compatible object
// storage" (odh-observability rhoai-3.7-ea.1 docs/usage-logs.md L29). The
// Observability tab gives it a bucket of its own on this tool's SeaweedFS,
// and an IAM user whose inline policy reaches that bucket only, made with the
// same IAM calls as the pipeline servers' users (provisionBucketUser).
//
// Ownership is by name: each setup draws names with a random suffix
// (NewUsageLogsNames) and records them on the DSCInitialization before it
// makes anything; a bucket or user of a freshly drawn name that exists
// already is refused, never adopted, and Clean up deletes exactly the
// recorded names. The user's name starts with "rnu-" but not with
// ScopedUserPrefix, so the leftover-key sweep (leftoverUsers) and the
// storage teardown's sweep of pipeline users (deleteScopedUsers) never take
// it. IAM writes need secured storage (S3IAMWritesArg).
const (
	usageLogsBucketPrefix = "rhoai-usage-logs-"
	usageLogsUserPrefix   = "rnu-usage-logs-"
	// UsageLogsSecret is the Secret in RHOAI's monitoring namespace that
	// holds the usage-logs key (the seven keys of docs/observability-modes.md
	// L300-328).
	UsageLogsSecret      = "rhoai-nightly-updater-usage-logs"
	usageLogsPolicyName  = "rnu-usage-logs"
	usageLogsPurposeTag  = "rnu-purpose"
	usageLogsPurpose     = "usage-logs"
	usageLogsProbeObject = "rhoai-nightly-updater-probe/usage-logs-key-check"
)

// usageLogsListPageLimit is how many objects one list (and one batch
// delete, S3's maximum) takes; tests shorten it.
var usageLogsListPageLimit = 1000

// ErrUsageLogsNameTaken: a bucket or user of a freshly drawn name exists
// already; nothing of this call is left (the bucket it made for the name is
// deleted again).
var ErrUsageLogsNameTaken = errors.New("a bucket or SeaweedFS user of the drawn name exists already, and this tool did not make it; nothing of it was used. Set up again draws another name")

// UsageLogsNames are the bucket and IAM user one setup makes.
type UsageLogsNames struct {
	Bucket string `json:"bucket"`
	User   string `json:"user"`
}

// Complete: both names are there.
func (n UsageLogsNames) Complete() bool { return n.Bucket != "" && n.User != "" }

// NewUsageLogsNames draws a bucket and a user name with one random suffix
// (crypto/rand, 6 hex characters).
func NewUsageLogsNames() (UsageLogsNames, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return UsageLogsNames{}, err
	}
	suffix := hex.EncodeToString(b)
	return UsageLogsNames{Bucket: usageLogsBucketPrefix + suffix, User: usageLogsUserPrefix + suffix}, nil
}

// UsageLogsPolicy is the usage-logs user's inline policy: list, read, write
// and delete objects of its bucket, nothing else (the policy of the
// pipeline servers' users, for another bucket).
func UsageLogsPolicy(bucket string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket","s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + bucket + `","arn:aws:s3:::` + bucket + `/*"]}]}`
}

// StorageSecured reports whether the S3 storage runs with IAM writes on
// (Secure S3 storage); false when it is not deployed.
func StorageSecured(c *kube.Client) (bool, error) { return storageSecured(c) }

// UsageLogsStorageBlock says why usage logs cannot be stored on the S3
// storage now, or "": it must be this tool's, deployed, ready and secured.
func UsageLogsStorageBlock(st types.ResourceState, secured bool) string {
	switch {
	case !st.Deployed || !st.ManagedByTool:
		return "Usage logs are kept in a bucket on this tool's S3 storage, which is not set up: set it up on the S3 storage tab first."
	case !st.Ready:
		return "Usage logs are kept in a bucket on this tool's S3 storage, which is not ready: " + kube.FirstNonEmpty(st.Message, "see the S3 storage tab") + "."
	case !secured:
		return "Usage logs get an S3 key of their own, which needs secured S3 storage (IAM writes on): use Secure S3 storage on the S3 storage tab first."
	}
	return ""
}

// ProvisionUsageLogsKey makes the recorded bucket and user (with its
// policy and tag) and the user's key (current when it is still the user's,
// else a new one: fresh), then checks the key: it can write, list, read and
// delete an object of the bucket, and cannot list the pipelines bucket.
// resume: the names were recorded by an earlier run of this setup, so a
// bucket or user of that name is the one it made; otherwise one that exists
// is refused.
func ProvisionUsageLogsKey(c *kube.Client, names UsageLogsNames, current S3Keys, resume bool) (keys S3Keys, fresh bool, err error) {
	sw, err := adminSeaweed(c)
	if err != nil {
		return S3Keys{}, false, err
	}
	status, body, err := sw.object(http.MethodPut, names.Bucket, "", nil, nil)
	switch {
	case err != nil:
		return S3Keys{}, false, fmt.Errorf("create bucket %s: %w", names.Bucket, err)
	case status == http.StatusConflict && !resume:
		return S3Keys{}, false, fmt.Errorf("bucket %s: %w", names.Bucket, ErrUsageLogsNameTaken)
	case status >= 300 && status != http.StatusConflict:
		return S3Keys{}, false, fmt.Errorf("create bucket %s: %w", names.Bucket, parseServiceError(status, body))
	}
	keys, fresh, err = provisionBucketUser(sw, bucketUser{name: names.User, policyName: usageLogsPolicyName, policy: UsageLogsPolicy(names.Bucket), tagKey: usageLogsPurposeTag, tagValue: usageLogsPurpose, mustCreate: !resume,
		foreign: func(other string) error {
			return fmt.Errorf("SeaweedFS user %s: %w", names.User, ErrUsageLogsNameTaken)
		}}, current)
	if errors.Is(err, ErrUsageLogsNameTaken) {
		// The bucket this call made for the name goes again (it is empty).
		if status, body, derr := sw.object(http.MethodDelete, names.Bucket, "", nil, nil); derr != nil || (status >= 300 && status != http.StatusNotFound) {
			if derr == nil {
				derr = parseServiceError(status, body)
			}
			return S3Keys{}, false, fmt.Errorf("%w; and bucket %s, which this setup made, could not be deleted again (%w): delete it in the SeaweedFS admin UI", err, names.Bucket, derr)
		}
		return S3Keys{}, false, err
	}
	if err != nil {
		return S3Keys{}, false, err
	}
	if err := probeUsageLogsKey(c.Context(), keys, names.Bucket); err != nil {
		return keys, fresh, fmt.Errorf("the new key of SeaweedFS user %s does not work as it should: %w", names.User, err)
	}
	return keys, fresh, nil
}

// probeUsageLogsKey checks keys against the storage: write, list, read and
// delete an object in bucket, and a refused listing of the pipelines bucket.
func probeUsageLogsKey(ctx context.Context, keys S3Keys, bucket string) error {
	sc := newSeaweedClient(ctx, s3ToolEndpoint(), keys)
	sc.http = s3SlowHTTPClient
	content := []byte("rhoai-nightly-updater usage-logs key check")
	steps := []struct {
		what, method, key string
		query             url.Values
		body              []byte
		ok                []int
	}{
		{"write a test object", http.MethodPut, usageLogsProbeObject, nil, content, []int{http.StatusOK}},
		{"list the bucket", http.MethodGet, "", url.Values{"list-type": {"2"}, "max-keys": {"1"}}, nil, []int{http.StatusOK}},
		{"read the test object", http.MethodGet, usageLogsProbeObject, nil, nil, []int{http.StatusOK}},
		{"delete the test object", http.MethodDelete, usageLogsProbeObject, nil, nil, []int{http.StatusOK, http.StatusNoContent}},
	}
	for _, s := range steps {
		status, body, err := sc.object(s.method, bucket, s.key, s.query, s.body)
		if err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
		if !slices.Contains(s.ok, status) {
			return fmt.Errorf("%s: %w", s.what, parseServiceError(status, body))
		}
		if s.method == http.MethodGet && s.key != "" && string(body) != string(content) {
			return fmt.Errorf("%s: it reads back other content", s.what)
		}
	}
	status, _, err := sc.object(http.MethodGet, s3Bucket, "", url.Values{"list-type": {"2"}, "max-keys": {"1"}}, nil)
	if err != nil {
		return fmt.Errorf("check that it cannot read bucket %s: %w", s3Bucket, err)
	}
	if status != http.StatusForbidden {
		return fmt.Errorf("it can list bucket %s (HTTP %d), so its policy does not limit it to bucket %s", s3Bucket, status, bucket)
	}
	return nil
}

// DeleteUsageLogsStorage deletes the recorded user (its keys first, as
// SeaweedFS refuses to delete a user with keys) and the recorded bucket
// with every object in it, each on its own: one that is gone already counts
// as deleted, so a run that stopped after one finishes the other. It
// returns what it did, one line each; on an error, what it did before.
func DeleteUsageLogsStorage(c *kube.Client, names UsageLogsNames) ([]string, error) {
	var logs []string
	if !names.Complete() {
		return logs, fmt.Errorf("this tool's record names no bucket or user")
	}
	sw, err := adminSeaweed(c)
	if err != nil {
		return logs, err
	}
	ids, found, err := sw.ListAccessKeys(names.User)
	if err != nil {
		return logs, fmt.Errorf("list the keys of SeaweedFS user %s: %w", names.User, err)
	}
	if found {
		for _, id := range ids {
			if err := sw.DeleteAccessKey(names.User, id); err != nil {
				return logs, fmt.Errorf("delete a key of SeaweedFS user %s: %w", names.User, err)
			}
		}
		if err := sw.DeleteUser(names.User); err != nil {
			return logs, fmt.Errorf("delete SeaweedFS user %s: %w", names.User, err)
		}
		logs = append(logs, fmt.Sprintf("OK: SeaweedFS user %s and its keys deleted", names.User))
	}
	n, gone, err := emptyBucket(sw, names.Bucket)
	if err != nil || gone {
		return logs, err
	}
	status, body, err := sw.object(http.MethodDelete, names.Bucket, "", nil, nil)
	if err != nil {
		return logs, fmt.Errorf("delete bucket %s: %w", names.Bucket, err)
	}
	if status >= 300 && status != http.StatusNotFound {
		return logs, fmt.Errorf("delete bucket %s: %w", names.Bucket, parseServiceError(status, body))
	}
	logs = append(logs, fmt.Sprintf("OK: bucket %s deleted with its %d %s", names.Bucket, n, kube.Verb(n, "object", "objects")))
	return logs, nil
}

// emptyBucket deletes every object of bucket, page by page (ListObjectsV2).
// gone is true when the bucket does not exist.
func emptyBucket(sw *seaweedClient, bucket string) (deleted int, gone bool, err error) {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {fmt.Sprint(usageLogsListPageLimit)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		status, body, err := sw.object(http.MethodGet, bucket, "", q, nil)
		if err != nil {
			return deleted, false, fmt.Errorf("list bucket %s: %w", bucket, err)
		}
		if status == http.StatusNotFound {
			return deleted, true, nil
		}
		if status >= 300 {
			return deleted, false, fmt.Errorf("list bucket %s: %w", bucket, parseServiceError(status, body))
		}
		var page struct {
			Keys        []string `xml:"Contents>Key"`
			IsTruncated bool     `xml:"IsTruncated"`
			Next        string   `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return deleted, false, fmt.Errorf("list bucket %s: parse the answer: %w", bucket, err)
		}
		if len(page.Keys) > 0 {
			if err := sw.deleteObjects(bucket, page.Keys); err != nil {
				return deleted, false, err
			}
			deleted += len(page.Keys)
		}
		if !page.IsTruncated || page.Next == "" {
			return deleted, false, nil
		}
		token = page.Next
	}
}

// UsageLogsSecretData is the Secret's seven keys, as the docs list them
// (docs/observability-modes.md L314-328; the DSCI CRD's description of
// usageLogs.storage.secretName lists the same): the in-cluster S3 address,
// plain HTTP inside the cluster (insecure "true"), and path-style requests
// (s3ForcePathStyle "true"), as SeaweedFS serves them.
func UsageLogsSecretData(keys S3Keys, bucket string) map[string]string {
	return map[string]string{
		"access_key_id":     keys.AccessKey,
		"access_key_secret": keys.SecretKey,
		"bucketnames":       bucket,
		"endpoint":          S3Endpoint(),
		"region":            S3Region,
		"insecure":          "true",
		"s3ForcePathStyle":  "true",
	}
}

// UsageLogsSecretKeys reads the usage-logs key from the Secret ns/name
// (found false when there is none).
func UsageLogsSecretKeys(c *kube.Client, ns string) (keys S3Keys, meta kube.ObjectMeta, found bool, err error) {
	var secret struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	found, err = c.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", ns, UsageLogsSecret), &secret)
	if err != nil || !found {
		return S3Keys{}, kube.ObjectMeta{}, found, err
	}
	return S3Keys{AccessKey: DecodeBase64Field(secret.Data["access_key_id"]), SecretKey: DecodeBase64Field(secret.Data["access_key_secret"])}, secret.Metadata, true, nil
}

// UsageLogsUseStorage reports whether RHOAI's monitoring keeps usage logs in
// this storage: the DSCInitialization's spec.monitoring.usageLogs.storage
// names the Secret this tool writes for it. Without a DSCInitialization API
// (no RHOAI) nothing does.
func UsageLogsUseStorage(c *kube.Client) (bool, error) {
	r, err := rhoai.ListServed(c, rhoai.DSCIGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", nil, rhoai.DSCIFallbackVersions)
	if err != nil {
		return false, fmt.Errorf("read the DSCInitialization: %w", err)
	}
	if r.Version == "" {
		return false, nil
	}
	var list struct {
		Items []struct {
			Spec struct {
				Monitoring struct {
					UsageLogs *struct {
						Storage *struct {
							SecretName string `json:"secretName"`
						} `json:"storage"`
					} `json:"usageLogs"`
				} `json:"monitoring"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return false, fmt.Errorf("parse the DSCInitialization: %w", err)
	}
	for _, d := range list.Items {
		if u := d.Spec.Monitoring.UsageLogs; u != nil && u.Storage != nil && u.Storage.SecretName == UsageLogsSecret {
			return true, nil
		}
	}
	return false, nil
}

// deleteObjects deletes keys of bucket in one request (S3 DeleteObjects, at
// most 1000 keys; quiet mode answers only the failures). An answer that is
// not a DeleteResult, or names keys it could not delete, is an error.
func (s *seaweedClient) deleteObjects(bucket string, keys []string) error {
	type object struct {
		Key string `xml:"Key"`
	}
	req := struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []object `xml:"Object"`
	}{Quiet: true}
	for _, k := range keys {
		req.Objects = append(req.Objects, object{Key: k})
	}
	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	sum := md5.Sum(body)
	status, resp, err := s.sendWith(http.MethodPost, "/"+bucket, url.Values{"delete": {""}}, "application/xml", body, "s3", false, map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])})
	if err != nil {
		return fmt.Errorf("delete objects of bucket %s: %w", bucket, err)
	}
	if status >= 300 {
		return fmt.Errorf("delete objects of bucket %s: %w", bucket, parseServiceError(status, resp))
	}
	var result struct {
		XMLName xml.Name `xml:"DeleteResult"`
		Errors  []struct {
			Key     string `xml:"Key"`
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	if err := xml.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("delete objects of bucket %s: the answer is not a DeleteResult: %w", bucket, err)
	}
	if len(result.Errors) > 0 {
		e := result.Errors[0]
		return fmt.Errorf("delete objects of bucket %s: %d of %d not deleted, the first %s (%s: %s)", bucket, len(result.Errors), len(keys), e.Key, e.Code, e.Message)
	}
	return nil
}
