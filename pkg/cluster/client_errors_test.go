package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A 403 answered to the ServiceAccount's token (a read, the app's records)
// points at its RBAC from an older template; one answered to the user's
// own token (every change) is about the user's RBAC, which its error says
// itself, so it gets no template hint.
func TestTemplateHintOnlyForTheServiceAccountsRefusals(t *testing.T) {
	sa := fmt.Errorf("read: %w", &K8sError{Status: 403, Reason: "Forbidden"})
	user := fmt.Errorf("patch: %w", &K8sError{Status: 403, Reason: "Forbidden", User: true})
	if templateHint(sa) == "" {
		t.Error("no hint for the ServiceAccount's 403")
	}
	if h := templateHint(user); h != "" {
		t.Errorf("hint for the user's 403: %q", h)
	}
	if !strings.Contains(user.Error(), UserForbiddenMessage) {
		t.Errorf("the user's 403 does not say whose permissions refused it: %v", user)
	}
	for _, err := range []error{nil, errors.New("x"), &K8sError{Status: 404}, &K8sError{Status: 401}} {
		if h := templateHint(err); h != "" {
			t.Errorf("%v: hint %q", err, h)
		}
	}
}

// A 401 to the user's own token while an operation runs is their sign-in
// ending mid-change ("sign_in_ended"), not the app's credentials
// ("unauthorized"), which the UI would send to the ServiceAccount.
func TestErrorCodeFromK8sErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("patch: %w", &K8sError{Status: 401, Reason: "Unauthorized", User: true}), "sign_in_ended"},
		{&K8sError{Status: 401, Reason: "Unauthorized"}, "unauthorized"},
		{fmt.Errorf("patch: %w", &K8sError{Status: 403, User: true}), "forbidden"},
		{&K8sError{Status: 403}, "forbidden"},
		{wrapNetworkError(errors.New("connection refused")), "network"},
		{&K8sError{Status: 500}, ""},
		{errors.New("x"), ""},
	} {
		if got := errorCodeFromK8sErr(tc.err); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
}

// End to end through an operation: creating the DSC with the user's token
// answered 401 reports sign_in_ended, and the message says what to do.
func TestUserTokenRejectedMidOperationIsSignInEnded(t *testing.T) {
	mockDSCSamples(t, func(*http.Request) (int, string) { return 200, testDSCSample })
	dscListPathV2 := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"
	dscListPathV1 := "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters"
	client, _, cleanup := newRecordingMockClient(map[string]mockResponse{
		namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""): csvListMock("3.6.0"),
		"GET " + dscListPathV2:  {body: `{"items":[]}`},
		dscListPathV1:           {body: `{"items":[]}`},
		"POST " + dscListPathV2: {body: `{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401,"message":"Unauthorized"}`, statusCode: 401},
	})
	defer cleanup()
	client.asUser = true

	result, err := CreateDefaultDSC(client)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ErrorCode != "sign_in_ended" || !strings.Contains(result.Message, "log in again") {
		t.Fatalf("result = %+v", result)
	}
}

// An operation that refuses because a cluster read failed reports the
// read's own code (CONTRIBUTING "Errors and operation results"); a failure
// that is not a cluster request keeps "prerequisites".
func TestFailedReadKeepsItsErrorCode(t *testing.T) {
	f, c := newFakeAPI(t)
	f.status("GET", "/apis/"+componentAPIGroup, http.StatusForbidden, "Forbidden")
	res, err := RevertDashboardImage(c)
	if err != nil || res.Success || res.ErrorCode != "forbidden" {
		t.Fatalf("result %+v, err %v", res, err)
	}
	f.mu.Lock()
	delete(f.routes, "GET /apis/"+componentAPIGroup)
	f.mu.Unlock()
	serveDashboardModule(f, fixtureAppsNS, fixtureOperatorName, `{"metadata":{}}`)
	res, err = RevertDashboardImage(c)
	if err != nil || res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("result %+v, err %v", res, err)
	}
}

func TestHTTPStatusForError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEC   string
	}{
		{"401", &K8sError{Status: 401}, 401, "unauthorized"},
		{"403 wrapped", fmt.Errorf("fetching DSC list: %w", &K8sError{Status: 403}), 403, "forbidden"},
		{"404", &K8sError{Status: 404}, 404, "not_found"},
		{"429 throttled by API priority and fairness", &K8sError{Status: 429}, 503, "rate_limited"},
		{"500", &K8sError{Status: 500}, 502, "cluster_unavailable"},
		{"503", &K8sError{Status: 503}, 502, "cluster_unavailable"},
		{"409", &K8sError{Status: 409}, 502, "cluster_error"},
		{"network", wrapNetworkError(errors.New("connection refused")), 502, "network"},
		{"timeout inside network error", wrapNetworkError(&url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}), 504, "timeout"},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), 503, "canceled"},
		{"quay rejects credentials", fmt.Errorf("x: %w", &quayAuthError{StatusCode: 401}), 502, "registry_auth"},
		{"quay outage", &quayAuthError{StatusCode: 503}, 502, "registry_unavailable"},
		{"other", errors.New("parse failure"), 500, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ec := HTTPStatusForError(tc.err)
			if code != tc.wantCode || ec != tc.wantEC {
				t.Fatalf("got (%d, %q), want (%d, %q)", code, ec, tc.wantCode, tc.wantEC)
			}
		})
	}
}
