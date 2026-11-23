package storage

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Known answers for the signer, computed apart from it (a small Python
// implementation of the AWS Signature Version 4 steps, with the example
// credentials of the AWS documentation): an S3 list with a query that
// needs encoding, and an IAM call with a form body.
func TestSignV4KnownAnswers(t *testing.T) {
	keys := S3Keys{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	cases := []struct {
		name, method, url, service, want string
		body                             []byte
	}{
		{"s3 list", http.MethodGet, "http://seaweedfs-s3.seaweedfs.svc:8333/pipelines?" + canonicalQuery(url.Values{"list-type": {"2"}, "prefix": {"a b~c"}, "max-keys": {"1"}}), "s3",
			"72338b26a0977685f67c79842e147b5eaa9f49e33ec6d61316360748fe19980a", nil},
		{"iam form", http.MethodPost, "http://seaweedfs-s3.seaweedfs.svc:8333/", "iam",
			"5423b61ba2eaa1ff10e232a250bd46161019b1bb6c19338dee35736bfb2a42d7",
			[]byte(canonicalQuery(url.Values{"Action": {"ListAccessKeys"}, "UserName": {"rnu-dspa-team-a"}, "Version": {"2010-05-08"}}))},
	}
	for _, tc := range cases {
		req, err := http.NewRequestWithContext(context.Background(), tc.method, tc.url, bytes.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		signV4(req, tc.body, keys.AccessKey, keys.SecretKey, tc.service, at)
		auth := req.Header.Get("Authorization")
		if !strings.HasSuffix(auth, "Signature="+tc.want) || !strings.Contains(auth, "Credential=AKIDEXAMPLE/20150830/us-east-1/"+tc.service+"/aws4_request") {
			t.Errorf("%s: Authorization = %s", tc.name, auth)
		}
	}
}
