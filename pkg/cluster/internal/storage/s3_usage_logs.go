package storage

import (
	"context"
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
	UsageLogsSecret        = "rhoai-nightly-updater-usage-logs"
	usageLogsPolicyName    = "rnu-usage-logs"
	usageLogsPurposeTag    = "rnu-purpose"
	usageLogsPurpose       = "usage-logs"
	usageLogsProbeObject   = "rhoai-nightly-updater-probe/usage-logs-key-check"
	usageLogsListPageLimit = 1000
)

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
	status, body, err := sw.object(http.MethodPut, UsageLogsBucket, "", nil, nil)
	if err != nil {
		return S3Keys{}, false, fmt.Errorf("create bucket %s: %w", UsageLogsBucket, err)
	}
	if status >= 300 && status != http.StatusConflict {
		return S3Keys{}, false, fmt.Errorf("create bucket %s: %w", UsageLogsBucket, parseServiceError(status, body))
	}
	keys, fresh, err = provisionBucketUser(sw, bucketUser{name: UsageLogsUser, policyName: usageLogsPolicyName, policy: UsageLogsPolicy, tagKey: usageLogsPurposeTag, tagValue: usageLogsPurpose,
		foreign: func(other string) error {
			return fmt.Errorf("SeaweedFS user %s is tagged %s=%s, not %s, so it is not the usage-logs user this tool makes; nothing was changed. Delete it in the SeaweedFS admin UI if nothing needs it", UsageLogsUser, usageLogsPurposeTag, other, usageLogsPurpose)
		}}, current)
	if err != nil {
		return S3Keys{}, false, err
	}
	if err := probeUsageLogsKey(c.Context(), keys); err != nil {
		return keys, fresh, fmt.Errorf("the new key of SeaweedFS user %s does not work as it should: %w", UsageLogsUser, err)
	}
	return keys, fresh, nil
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
// SeaweedFS refuses to delete a user with keys) and the bucket with every
// object in it. What is gone already counts as deleted. It returns what it
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
			return logs, fmt.Errorf("SeaweedFS user %s is not tagged %s=%s, so it is not the usage-logs user this tool made; it was not deleted", UsageLogsUser, usageLogsPurposeTag, usageLogsPurpose)
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
	n, gone, err := emptyBucket(sw, UsageLogsBucket)
	if err != nil {
		return logs, err
	}
	if gone {
		return logs, nil
	}
	status, body, err := sw.object(http.MethodDelete, UsageLogsBucket, "", nil, nil)
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
		for _, key := range page.Keys {
			status, body, err := sw.object(http.MethodDelete, bucket, key, nil, nil)
			if err != nil {
				return deleted, false, fmt.Errorf("delete an object of bucket %s: %w", bucket, err)
			}
			if status >= 300 && status != http.StatusNotFound {
				return deleted, false, fmt.Errorf("delete an object of bucket %s: %w", bucket, parseServiceError(status, body))
			}
			deleted++
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
