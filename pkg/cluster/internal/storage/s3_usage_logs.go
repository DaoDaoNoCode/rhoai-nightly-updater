package storage

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
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
// same IAM calls as the pipeline servers' users (provisionBucketUser). Its
// name starts with "rnu-" but not with ScopedUserPrefix, so the leftover-key
// sweep (leftoverUsers) and the storage teardown's sweep of pipeline users
// (deleteScopedUsers) never take it: only the Observability tab's Clean up
// deletes it. IAM writes need secured storage (S3IAMWritesArg).
const (
	UsageLogsBucket = "rhoai-usage-logs"
	UsageLogsUser   = "rnu-usage-logs"
	// UsageLogsSecret is the Secret in RHOAI's monitoring namespace that
	// holds the usage-logs key (the seven keys of docs/observability-modes.md
	// L300-328).
	UsageLogsSecret      = "rhoai-nightly-updater-usage-logs"
	usageLogsPolicyName  = "rnu-usage-logs"
	usageLogsPurposeTag  = "rnu-purpose"
	usageLogsPurpose     = "usage-logs"
	usageLogsProbeObject = "rhoai-nightly-updater-probe/usage-logs-key-check"
	// usageLogsBucketMarker is the object this tool writes into the bucket
	// right after it made it: what tells this tool's bucket from another of
	// the same name (Set up adopts, and Clean up empties and deletes, only a
	// bucket that has it).
	usageLogsBucketMarker = "rhoai-nightly-updater/bucket-owner"
)

// usageLogsListPageLimit is how many objects one list (and one batch
// delete, S3's maximum) takes; tests shorten it.
var usageLogsListPageLimit = 1000

// UsageLogsPolicy is the usage-logs user's inline policy: list, read, write
// and delete objects of its bucket, nothing else (the policy of the
// pipeline servers' users, for another bucket).
const UsageLogsPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket","s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::` + UsageLogsBucket + `","arn:aws:s3:::` + UsageLogsBucket + `/*"]}]}`

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

// ProvisionUsageLogsKey makes the usage-logs bucket (kept when it exists),
// the user with its policy and tag, and its key (current when it is still
// the user's, else a new one: fresh), then checks the key: it can write,
// list, read and delete an object of the bucket, and cannot list the
// pipelines bucket (the policy's scope).
func ProvisionUsageLogsKey(c *kube.Client, current S3Keys) (keys S3Keys, fresh bool, err error) {
	sw, err := adminSeaweed(c)
	if err != nil {
		return S3Keys{}, false, err
	}
	if err := ensureUsageLogsBucket(sw); err != nil {
		return S3Keys{}, false, err
	}
	keys, fresh, err = provisionBucketUser(sw, bucketUser{name: UsageLogsUser, policyName: usageLogsPolicyName, policy: UsageLogsPolicy, tagKey: usageLogsPurposeTag, tagValue: usageLogsPurpose, requireTag: true,
		foreign: func(other string) error {
			return fmt.Errorf("SeaweedFS user %s exists and is not tagged %s=%s (it has %q), so this tool did not make it and does not use it; nothing was changed. Delete it in the SeaweedFS admin UI if nothing needs it", UsageLogsUser, usageLogsPurposeTag, usageLogsPurpose, other)
		}}, current)
	if err != nil {
		return S3Keys{}, false, err
	}
	if err := probeUsageLogsKey(c.Context(), keys); err != nil {
		return keys, fresh, fmt.Errorf("the new key of SeaweedFS user %s does not work as it should: %w", UsageLogsUser, err)
	}
	return keys, fresh, nil
}

// ensureUsageLogsBucket makes the bucket and writes this tool's marker into
// it; a bucket that exists is used only when it has the marker.
func ensureUsageLogsBucket(sw *seaweedClient) error {
	status, body, err := sw.object(http.MethodPut, UsageLogsBucket, "", nil, nil)
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", UsageLogsBucket, err)
	}
	switch {
	case status == http.StatusConflict:
		ours, err := bucketMarked(sw)
		if err != nil {
			return err
		}
		if !ours {
			return fmt.Errorf("bucket %s exists and has no object %s, so this tool did not make it and does not use it; nothing was changed. Delete or rename it in the SeaweedFS admin UI if nothing needs it", UsageLogsBucket, usageLogsBucketMarker)
		}
		return nil
	case status >= 300:
		return fmt.Errorf("create bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
	}
	status, body, err = sw.object(http.MethodPut, UsageLogsBucket, usageLogsBucketMarker, nil, []byte("made by rhoai-nightly-updater for RHOAI's usage logs (the Observability tab)"))
	if err != nil {
		return fmt.Errorf("mark bucket %s: %w", UsageLogsBucket, err)
	}
	if status >= 300 {
		return fmt.Errorf("mark bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
	}
	return nil
}

// bucketMarked reports whether the usage-logs bucket has this tool's marker.
func bucketMarked(sw *seaweedClient) (bool, error) {
	status, body, err := sw.object(http.MethodHead, UsageLogsBucket, usageLogsBucketMarker, nil, nil)
	switch {
	case err != nil:
		return false, fmt.Errorf("read bucket %s: %w", UsageLogsBucket, err)
	case status == http.StatusOK:
		return true, nil
	case status == http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("read bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
}

// probeUsageLogsKey checks keys against the storage: write, list, read and
// delete an object in the usage-logs bucket, and a refused listing of the
// pipelines bucket.
func probeUsageLogsKey(ctx context.Context, keys S3Keys) error {
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
		status, body, err := sc.object(s.method, UsageLogsBucket, s.key, s.query, s.body)
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
		return fmt.Errorf("it can list bucket %s (HTTP %d), so its policy does not limit it to bucket %s", s3Bucket, status, UsageLogsBucket)
	}
	return nil
}

// UsageLogsKeyRegistered reports whether keyID is still a key of the
// usage-logs user (false when the user is gone).
func UsageLogsKeyRegistered(c *kube.Client, keyID string) (bool, error) {
	sw, err := adminSeaweed(c)
	if err != nil {
		return false, err
	}
	ids, _, err := sw.ListAccessKeys(UsageLogsUser)
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, keyID), nil
}

// DeleteUsageLogsStorage deletes the usage-logs user (its keys first, as
// SeaweedFS refuses to delete a user with keys) when it carries this tool's
// tag, and the bucket with every object in it when it has this tool's
// marker: each on its own proof, so a run that stopped after one finishes
// the other. What is gone already counts as deleted. It returns what it
// did, one line each; on an error, what it did before.
func DeleteUsageLogsStorage(c *kube.Client) ([]string, error) {
	var logs []string
	sw, err := adminSeaweed(c)
	if err != nil {
		return logs, err
	}
	tags, found, err := sw.ListUserTags(UsageLogsUser)
	if err != nil {
		return logs, fmt.Errorf("read SeaweedFS user %s: %w", UsageLogsUser, err)
	}
	if found {
		if got := tags[usageLogsPurposeTag]; got != usageLogsPurpose {
			return logs, fmt.Errorf("SeaweedFS user %s is not tagged %s=%s, so this tool did not make it; it was not deleted", UsageLogsUser, usageLogsPurposeTag, usageLogsPurpose)
		}
		ids, _, err := sw.ListAccessKeys(UsageLogsUser)
		if err != nil {
			return logs, fmt.Errorf("list the keys of SeaweedFS user %s: %w", UsageLogsUser, err)
		}
		for _, id := range ids {
			if err := sw.DeleteAccessKey(UsageLogsUser, id); err != nil {
				return logs, fmt.Errorf("delete a key of SeaweedFS user %s: %w", UsageLogsUser, err)
			}
		}
		if err := sw.DeleteUser(UsageLogsUser); err != nil {
			return logs, fmt.Errorf("delete SeaweedFS user %s: %w", UsageLogsUser, err)
		}
		logs = append(logs, fmt.Sprintf("OK: SeaweedFS user %s and its keys deleted", UsageLogsUser))
	}
	status, body, err := sw.object(http.MethodHead, UsageLogsBucket, "", nil, nil)
	switch {
	case err != nil:
		return logs, fmt.Errorf("read bucket %s: %w", UsageLogsBucket, err)
	case status == http.StatusNotFound:
		return logs, nil
	case status >= 300:
		return logs, fmt.Errorf("read bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
	}
	ours, err := bucketMarked(sw)
	if err != nil {
		return logs, err
	}
	if !ours {
		return logs, fmt.Errorf("bucket %s has no object %s, so this tool did not make it; it was not emptied or deleted", UsageLogsBucket, usageLogsBucketMarker)
	}
	n, gone, err := emptyBucket(sw, UsageLogsBucket)
	if err != nil {
		return logs, err
	}
	if gone {
		return logs, nil
	}
	status, body, err = sw.object(http.MethodDelete, UsageLogsBucket, "", nil, nil)
	if err != nil {
		return logs, fmt.Errorf("delete bucket %s: %w", UsageLogsBucket, err)
	}
	if status >= 300 && status != http.StatusNotFound {
		return logs, fmt.Errorf("delete bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
	}
	logs = append(logs, fmt.Sprintf("OK: bucket %s deleted with its %d %s", UsageLogsBucket, n, kube.Verb(n, "object", "objects")))
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
func UsageLogsSecretData(keys S3Keys) map[string]string {
	return map[string]string{
		"access_key_id":     keys.AccessKey,
		"access_key_secret": keys.SecretKey,
		"bucketnames":       UsageLogsBucket,
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
// most 1000 keys; quiet mode answers only the failures).
func (s *seaweedClient) deleteObjects(bucket string, keys []string) error {
	type object struct {
		Key string `xml:"Key"`
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []object `xml:"Object"`
	}{Quiet: true, Objects: func() []object {
		out := make([]object, len(keys))
		for i, k := range keys {
			out[i] = object{Key: k}
		}
		return out
	}()})
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
		Errors []struct {
			Key     string `xml:"Key"`
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	if err := xml.Unmarshal(resp, &result); err == nil && len(result.Errors) > 0 {
		e := result.Errors[0]
		return fmt.Errorf("delete objects of bucket %s: %d not deleted, the first %s: %s %s", bucket, len(result.Errors), e.Key, e.Code, e.Message)
	}
	return nil
}
