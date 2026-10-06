package cluster

import "encoding/base64"

// exampleAuth returns the "auth" value of a docker config entry (base64 of
// "user:password") for fake credentials. Tests build these values at run time
// so the repository holds no literal that secret scanners read as a registry
// credential.
func exampleAuth(userPass string) string {
	return base64.StdEncoding.EncodeToString([]byte(userPass))
}
