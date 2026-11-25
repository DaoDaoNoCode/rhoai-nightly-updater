package api

import (
	"os"
	"sync/atomic"
)

// DevConfig is the local development setup (dev.sh): the backend runs on
// the developer's machine, without oauth-proxy in front, with the
// developer's own token. It is read from the environment once, when the
// process starts; in the cluster DEV_MODE is unset.
type DevConfig struct {
	// Mode is DEV_MODE=true: a bearer token is accepted (there is no
	// proxy to forward the user's token), DEV_USER and DEV_TOKEN apply,
	// and main serves only this machine.
	Mode bool
	// Token is DEV_TOKEN: the user token without a bearer header, and the
	// cluster token when no ServiceAccount token is mounted. Mode only.
	Token string
	// User is DEV_USER: the user name when nothing else names one. Mode
	// only.
	User string
	// Role is DEV_ROLE. "user" turns off the developer's cluster-admin
	// bypass of the mutation gate and of the sign-in check
	// (devAdminBypass), so the request's own token decides the role and
	// a project action runs its preflight: the non-admin path, with a
	// non-admin's bearer token. Mode only, without a ServiceAccount token.
	Role string
}

// DevRoleUser is DEV_ROLE's value that turns the developer's bypass off.
const DevRoleUser = "user"

// devConfig is the setup read at startup; tests replace it (setDev).
var devConfig atomic.Pointer[DevConfig]

func init() {
	devConfig.Store(readDevConfig(os.Getenv))
}

func readDevConfig(getenv func(string) string) *DevConfig {
	return &DevConfig{
		Mode:  getenv("DEV_MODE") == "true",
		Token: getenv("DEV_TOKEN"),
		User:  getenv("DEV_USER"),
		Role:  getenv("DEV_ROLE"),
	}
}

// Dev is the development setup read when the process started.
func Dev() DevConfig {
	return *devConfig.Load()
}
