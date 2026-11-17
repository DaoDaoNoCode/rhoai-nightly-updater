package cluster

import "testing"

func TestMLflowUIURL(t *testing.T) {
	for _, tc := range []struct{ name, statusURL, route, want string }{
		{"gateway URL from the CR (current releases)", "https://rh-ai.apps.example.com/mlflow", "", "https://rh-ai.apps.example.com/mlflow"},
		{"trailing slash trimmed", "https://rh-ai.apps.example.com/mlflow/", "", "https://rh-ai.apps.example.com/mlflow"},
		{"CR URL wins over a Route", "https://rh-ai.apps.example.com/mlflow", "mlflow-redhat-ods-applications.apps.example.com", "https://rh-ai.apps.example.com/mlflow"},
		{"no CR URL: the Route (older releases)", "", "mlflow-redhat-ods-applications.apps.example.com", "https://mlflow-redhat-ods-applications.apps.example.com"},
		{"non-https CR URL is not used", "http://mlflow.redhat-ods-applications.svc:8443", "", ""},
		{"neither", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mlflowUIURL(tc.statusURL, tc.route); got != tc.want {
				t.Errorf("mlflowUIURL(%q, %q) = %q, want %q", tc.statusURL, tc.route, got, tc.want)
			}
		})
	}
}
