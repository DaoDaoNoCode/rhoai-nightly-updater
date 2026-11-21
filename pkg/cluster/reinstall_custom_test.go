package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func TestCustomReinstallPinsOldBuildAndSelectsOlderChannel(t *testing.T) {
	for _, tc := range []struct{ target, override string }{{"custom", ""}, {"custom", "beta"}, {"nightly", ""}} {
		t.Run(tc.target+" override="+tc.override, func(t *testing.T) {
			override := tc.override
			image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.3@sha256:" + strings.Repeat("a", 64)
			csPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, rhoai.CatalogName)
			subPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName)
			var appliedImage, appliedChannel string
			var deletedCSV bool
			// A moved tag must not be consulted for custom reinstall.
			t.Cleanup(registry.SetHTTPClientForTest(&http.Client{Transport: dscSampleTransport(func(*http.Request) (*http.Response, error) {
				t.Error("custom reinstall attempted to resolve the current tag instead of respecting the digest")
				return nil, context.Canceled
			})}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if dashboardOperatorAbsent(w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/catalogsources/"+rhoai.CatalogName+"-verify-"):
					io.WriteString(w, `{"status":{"connectionState":{"lastObservedState":"READY"}}}`)
				case r.Method == "PATCH" && r.URL.Path == csPath:
					var body struct {
						Spec struct {
							Image string `json:"image"`
						} `json:"spec"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					appliedImage = body.Spec.Image
					io.WriteString(w, `{}`)
				case r.Method == "PATCH" && r.URL.Path == subPath:
					var body struct {
						Spec struct {
							Channel string `json:"channel"`
						} `json:"spec"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					appliedChannel = body.Spec.Channel
					io.WriteString(w, `{}`)
				case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/clusterserviceversions/"):
					deletedCSV = true
					io.WriteString(w, `{}`)
				case r.URL.Path == subPath:
					// Starting on stable must not short-circuit a custom reinstall.
					io.WriteString(w, `{"spec":{"source":"`+getStableSource()+`","channel":"stable-3.6"},"status":{"currentCSV":"rhods-operator.3.6.0","installedCSV":"rhods-operator.3.6.0","installPlanRef":{"name":"old-build-install"}}}`)
				case r.URL.Path == csPath:
					io.WriteString(w, `{"spec":{"image":"`+image+`"},"status":{"connectionState":{"lastObservedState":"READY"}}}`)
				case strings.Contains(r.URL.Path, "/clusterserviceversions/"):
					io.WriteString(w, `{"metadata":{"name":"rhods-operator.3.6.0"},"spec":{"version":"3.6.0"},"status":{"phase":"Succeeded"}}`)
				case strings.HasSuffix(r.URL.Path, "/clusterserviceversions"):
					io.WriteString(w, csvListMock("3.6.0").body)
				case strings.HasSuffix(r.URL.Path, "/packagemanifests"):
					io.WriteString(w, `{"items":[{"metadata":{"name":"rhods-operator"},"status":{"packageName":"rhods-operator","catalogSource":"`+strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "catalog=")+`","catalogSourceNamespace":"`+rhoai.CatalogNS+`","channels":[{"name":"stable-3.6","currentCSV":"rhods-operator.v3.6.0"},{"name":"stable-3.3","currentCSV":"rhods-operator.v3.3.0"},{"name":"beta","currentCSV":"rhods-operator.v3.3.0-ea.1"}]}}]}`)
				default:
					io.WriteString(w, `{"items":[]}`)
				}
			}))
			defer server.Close()
			c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
			// 3.6.0 -> 3.3 is a downgrade, which needs explicit confirmation.
			if refused, _ := Reinstall(c, tc.target, image, override); refused.ErrorCode != errorCodeDowngrade || deletedCSV {
				t.Fatalf("unconfirmed downgrade: %+v deletedCSV=%v", refused, deletedCSV)
			}
			result, err := ReinstallWithOptions(c, tc.target, image, override, OperationOptions{AllowDowngrade: true})
			if err != nil || !result.Success {
				t.Fatalf("reinstall: %+v, %v", result, err)
			}
			wantChannel := "stable-3.3"
			if override != "" {
				wantChannel = override
			}
			if appliedImage != image || appliedChannel != wantChannel || !deletedCSV {
				t.Fatalf("image=%q channel=%q deletedCSV=%v", appliedImage, appliedChannel, deletedCSV)
			}
		})
	}
}
