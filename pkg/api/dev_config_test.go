package api

import "testing"

// Only DEV_MODE=true turns the development setup on; DEV_TOKEN and DEV_USER
// are kept as given (every reader checks Mode first).
func TestReadDevConfig(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want DevConfig
	}{
		{map[string]string{}, DevConfig{}},
		{map[string]string{"DEV_MODE": "1", "DEV_TOKEN": "tok"}, DevConfig{Token: "tok"}},
		{map[string]string{"DEV_MODE": "true", "DEV_TOKEN": "tok", "DEV_USER": "dev"}, DevConfig{Mode: true, Token: "tok", User: "dev"}},
	} {
		if got := *readDevConfig(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("env %v: got %+v, want %+v", tc.env, got, tc.want)
		}
	}
}
