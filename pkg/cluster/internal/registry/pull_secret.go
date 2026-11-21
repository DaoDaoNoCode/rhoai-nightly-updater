package registry

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// quayCredentialCacheTTL bounds how long a Quay verdict on a credential is
// reused. Status is polled every few seconds; without a cache every poll calls
// Quay's token endpoint.
const quayCredentialCacheTTL = 5 * time.Minute

type quayCredentialVerdict struct {
	err error // nil when accepted; a rejection error otherwise
	at  time.Time
}

var (
	quayCredentialCacheMu sync.Mutex
	quayCredentialCache   = map[[sha256.Size]byte]quayCredentialVerdict{}
)

// verifyQuayCredentials confirms Quay accepts the credentials. Tests override
// it to skip the real network call.
var verifyQuayCredentials = VerifyQuayCredentialsCached

// VerifyQuayCredentials asks Quay whether it accepts the credentials
// (through verifyQuayCredentials, which tests replace).
func VerifyQuayCredentials(basicAuth string) error { return verifyQuayCredentials(basicAuth) }

// VerifyQuayCredentialsCached calls the Quay token endpoint to confirm the
// credentials are accepted. Definitive answers (accepted or rejected) are cached
// per credential; transient failures are not.
func VerifyQuayCredentialsCached(basicAuth string) error {
	key := sha256.Sum256([]byte(basicAuth))
	quayCredentialCacheMu.Lock()
	cached, ok := quayCredentialCache[key]
	quayCredentialCacheMu.Unlock()
	if ok && time.Since(cached.at) < quayCredentialCacheTTL {
		return cached.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := QuayBearerToken(ctx, quayHTTPClient, basicAuth)
	if err == nil || IsCredentialRejection(err) {
		quayCredentialCacheMu.Lock()
		for k, v := range quayCredentialCache {
			if time.Since(v.at) >= quayCredentialCacheTTL {
				delete(quayCredentialCache, k)
			}
		}
		quayCredentialCache[key] = quayCredentialVerdict{err: err, at: time.Now()}
		quayCredentialCacheMu.Unlock()
	}
	return err
}

func PullSecret(c *kube.Client) (types.PullSecretInfo, error) {
	info, _, err := ReadPullSecret(c)
	return info, err
}

// ReadPullSecret validates the pull secret and also returns the quay.io/rhoai
// credential it holds, so status polling does not read the Secret twice.
func ReadPullSecret(c *kube.Client) (types.PullSecretInfo, string, error) {
	path := rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, exists, err := c.GetOptional(path)
	if err != nil {
		return types.PullSecretInfo{Exists: false}, "", err
	}
	if !exists {
		return types.PullSecretInfo{Exists: false, Detail: "Secret not found"}, "", nil
	}

	dockerConfig, problem := DecodeDockerConfigSecret(body)
	if problem != "" {
		return types.PullSecretInfo{Exists: true, Detail: problem}, "", nil
	}

	auths, ok := dockerConfig["auths"].(map[string]any)
	if !ok {
		return types.PullSecretInfo{Exists: true, Detail: "Missing 'auths' key in docker config"}, "", nil
	}

	// Validate the same credential that registry calls use.
	basicAuth := SelectQuayAuth(auths)
	if basicAuth == "" {
		return types.PullSecretInfo{Exists: true, Detail: "No quay.io/rhoai entry found in auths"}, "", nil
	}

	if err := verifyQuayCredentials(basicAuth); err != nil {
		if IsCredentialRejection(err) {
			return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Credentials rejected by Quay: %v", err)}, basicAuth, nil
		}
		return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Could not verify credentials with Quay: %v", err)}, basicAuth, nil
	}

	return types.PullSecretInfo{Exists: true, Valid: true}, basicAuth, nil
}
