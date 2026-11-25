package cluster

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// seaweedFake stands in for a minimal SeaweedFS during scoped-key tests, implementing
// the embedded IAM API (POST / with Action in the form body) plus the S3 object
// operations the tool issues. Kubernetes-fake objects feed it the way a live pod
// feeds a real server: the admin key is whatever Secret seaweedfs-s3-credentials
// holds at the moment, and IAM mutations succeed only when the storage Deployment
// uses secured args. A user key can touch objects in bucket pipelines after that
// user receives a policy; IAM and bucket writes stay forbidden for that key.
type seaweedFake struct {
	t   *testing.T
	f   *resourceFake
	mu  sync.Mutex
	srv *httptest.Server
	// name -> user record.
	users map[string]*fakeIAMUser
	// "bucket/key" -> payload.
	objects map[string][]byte
	// buckets that exist (pipelines always does).
	buckets map[string]bool
	nextKey int
	// when set, failIAM forces the given IAM action to return that HTTP status.
	failIAM map[string]int
	// per-user override for object-call HTTP status (simulates an invalid key).
	failUserObjects map[string]int
	// adminSecretOverride, when set, is the admin secret the server
	// expects instead of the Secret's (a client signing with another one).
	adminSecretOverride string
	// failDeleteUser makes DeleteUser of that user fail.
	failDeleteUser string
	// audit trail: "ACTION user" on IAM, "METHOD path (who)" on S3.
	calls []string
}

type fakeIAMUser struct {
	keys   map[string]string // access key ID maps to secret
	order  []string
	policy string
	tags   map[string]string
}

// newSeaweedFake spins up the stub and routes the tool's S3 and IAM traffic through it.
func newSeaweedFake(t *testing.T, f *resourceFake) *seaweedFake {
	t.Helper()
	sw := &seaweedFake{t: t, f: f, users: map[string]*fakeIAMUser{}, objects: map[string][]byte{}, failIAM: map[string]int{}, failUserObjects: map[string]int{}}
	sw.srv = httptest.NewServer(http.HandlerFunc(sw.serve))
	t.Cleanup(sw.srv.Close)
	t.Cleanup(storage.SetS3ToolEndpointForTest(sw.srv.URL))
	return sw
}

// adminKeyID pulls the access key from the storage Secret—the same value the pod sees.
func (sw *seaweedFake) adminKeyID() string { return sw.adminField(storage.S3AccessKeyField) }

// adminField reads one key of the storage Secret, as the pod has it.
func (sw *seaweedFake) adminField(field string) string {
	obj := sw.f.get(s3SecretPath)
	if obj == nil {
		return ""
	}
	data, _ := obj["data"].(map[string]any)
	raw, _ := data[field].(string)
	b, _ := base64.StdEncoding.DecodeString(raw)
	return string(b)
}

// secured reports whether the storage Deployment container args are in secured mode.
func (sw *seaweedFake) secured() bool {
	obj := sw.f.get(s3DeployPath)
	if obj == nil {
		return false
	}
	ctrs, _ := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if len(ctrs) == 0 {
		return false
	}
	var args []string
	for _, a := range ctrs[0].(map[string]any)["args"].([]any) {
		args = append(args, a.(string))
	}
	return storage.ArgsSecured(args)
}

// userOf looks up which user owns a given access key ID.
func (sw *seaweedFake) userOf(id string) string {
	for name, u := range sw.users {
		if _, ok := u.keys[id]; ok {
			return name
		}
	}
	return ""
}

// keysOf lists that user's access key IDs in the order they were created.
func (sw *seaweedFake) keysOf(user string) []string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if u := sw.users[user]; u != nil {
		return slices.Clone(u.order)
	}
	return nil
}

func (sw *seaweedFake) userNames() []string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	var out []string
	for n := range sw.users {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (sw *seaweedFake) iamCalls() []string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	var out []string
	for _, c := range sw.calls {
		if !strings.Contains(c, "/") {
			out = append(out, c)
		}
	}
	return out
}

func (sw *seaweedFake) xmlError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<ErrorResponse><Error><Code>%s</Code><Message>%s</Message></Error></ErrorResponse>`, code, code)
}

func (sw *seaweedFake) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	id := ""
	if i := strings.Index(auth, "Credential="); i >= 0 {
		id = strings.SplitN(auth[i+len("Credential="):], "/", 2)[0]
	}
	admin := sw.adminKeyID()
	adminSecret := sw.adminField(storage.S3SecretKeyField)
	secured := sw.secured()
	body, _ := io.ReadAll(r.Body)
	sw.mu.Lock()
	defer sw.mu.Unlock()
	user := sw.userOf(id)
	isAdmin := id != "" && id == admin
	if !isAdmin && user == "" {
		sw.calls = append(sw.calls, r.Method+" "+r.URL.Path+" (unknown key)")
		sw.xmlError(w, http.StatusForbidden, "InvalidAccessKeyId")
		return
	}
	secret := adminSecret
	if sw.adminSecretOverride != "" {
		secret = sw.adminSecretOverride
	}
	if !isAdmin {
		secret = sw.users[user].keys[id]
	}
	if err := verifySigV4(r, body, id, secret); err != nil {
		sw.calls = append(sw.calls, r.Method+" "+r.URL.Path+" (bad signature: "+err.Error()+")")
		sw.xmlError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/" {
		form, _ := url.ParseQuery(string(body))
		sw.iam(w, form, isAdmin, secured)
		return
	}
	who := "admin"
	if !isAdmin {
		who = user
	}
	sw.calls = append(sw.calls, r.Method+" "+r.URL.Path+" ("+who+")")
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	bucket, key := parts[0], ""
	if len(parts) == 2 {
		key = parts[1]
	}
	if !isAdmin {
		if code := sw.failUserObjects[user]; code != 0 {
			sw.xmlError(w, code, "InternalError")
			return
		}
		// A user reaches the buckets its inline policy names, objects and
		// listing only.
		if p := sw.users[user].policy; p == "" || !strings.Contains(p, `"arn:aws:s3:::`+bucket+`"`) || (key == "" && r.Method != http.MethodGet) {
			sw.xmlError(w, http.StatusForbidden, "AccessDenied")
			return
		}
	}
	if sw.buckets == nil {
		sw.buckets = map[string]bool{"pipelines": true}
	}
	switch {
	case key == "" && r.Method == http.MethodPut:
		sw.buckets[bucket] = true
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodDelete:
		if !sw.buckets[bucket] {
			sw.xmlError(w, http.StatusNotFound, "NoSuchBucket")
			return
		}
		for k := range sw.objects {
			if strings.HasPrefix(k, bucket+"/") {
				sw.xmlError(w, http.StatusConflict, "BucketNotEmpty")
				return
			}
		}
		delete(sw.buckets, bucket)
		w.WriteHeader(http.StatusNoContent)
	case key == "" && r.Method == http.MethodGet:
		if !sw.buckets[bucket] {
			sw.xmlError(w, http.StatusNotFound, "NoSuchBucket")
			return
		}
		var keys []string
		for k := range sw.objects {
			if rest, ok := strings.CutPrefix(k, bucket+"/"); ok && strings.HasPrefix(rest, r.URL.Query().Get("prefix")) {
				keys = append(keys, rest)
			}
		}
		sort.Strings(keys)
		out := "<ListBucketResult><Name>" + bucket + "</Name>"
		for _, k := range keys {
			out += "<Contents><Key>" + k + "</Key></Contents>"
		}
		fmt.Fprint(w, out+"<IsTruncated>false</IsTruncated></ListBucketResult>")
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		data, ok := sw.objects[bucket+"/"+key]
		if !ok {
			sw.xmlError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case r.Method == http.MethodPut:
		sw.objects[bucket+"/"+key] = body
	case r.Method == http.MethodDelete:
		delete(sw.objects, bucket+"/"+key)
		w.WriteHeader(http.StatusNoContent)
	default:
		sw.xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (sw *seaweedFake) iam(w http.ResponseWriter, form url.Values, isAdmin, secured bool) {
	action, name := form.Get("Action"), form.Get("UserName")
	sw.calls = append(sw.calls, action+" "+name)
	write := action != "ListUsers" && action != "ListAccessKeys" && action != "ListUserTags"
	switch {
	case !isAdmin:
		sw.xmlError(w, http.StatusForbidden, "AccessDenied")
		return
	case write && !secured:
		sw.xmlError(w, http.StatusForbidden, "AccessDenied")
		return
	case sw.failIAM[action] != 0:
		sw.xmlError(w, sw.failIAM[action], "ServiceFailure")
		return
	}
	u := sw.users[name]
	if u == nil && action != "CreateUser" && action != "ListUsers" {
		sw.xmlError(w, http.StatusNotFound, "NoSuchEntity")
		return
	}
	type member struct {
		UserName    string `xml:"UserName,omitempty"`
		AccessKeyID string `xml:"AccessKeyId,omitempty"`
	}
	switch action {
	case "CreateUser":
		if u != nil {
			sw.xmlError(w, http.StatusConflict, "EntityAlreadyExists")
			return
		}
		sw.users[name] = &fakeIAMUser{keys: map[string]string{}, tags: map[string]string{}}
		fmt.Fprint(w, `<CreateUserResponse/>`)
	case "DeleteUser":
		if name == sw.failDeleteUser {
			sw.xmlError(w, http.StatusInternalServerError, "ServiceFailure")
			return
		}
		if len(u.keys) > 0 {
			sw.xmlError(w, http.StatusConflict, "DeleteConflict")
			return
		}
		delete(sw.users, name)
		fmt.Fprint(w, `<DeleteUserResponse/>`)
	case "PutUserPolicy":
		u.policy = form.Get("PolicyDocument")
		fmt.Fprint(w, `<PutUserPolicyResponse/>`)
	case "ListUserTags":
		type tag struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		}
		var tags []tag
		for k, v := range u.tags {
			tags = append(tags, tag{k, v})
		}
		out, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"ListUserTagsResponse"`
			Tags    []tag    `xml:"ListUserTagsResult>Tags>member"`
		}{Tags: tags})
		_, _ = w.Write(out)
	case "TagUser":
		u.tags[form.Get("Tags.member.1.Key")] = form.Get("Tags.member.1.Value")
		fmt.Fprint(w, `<TagUserResponse/>`)
	case "CreateAccessKey":
		sw.nextKey++
		id, secret := fmt.Sprintf("SCOPED%04d", sw.nextKey), fmt.Sprintf("scoped-secret-%d", sw.nextKey)
		u.keys[id] = secret
		u.order = append(u.order, id)
		fmt.Fprintf(w, `<CreateAccessKeyResponse><CreateAccessKeyResult><AccessKey><UserName>%s</UserName><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey></AccessKey></CreateAccessKeyResult></CreateAccessKeyResponse>`, name, id, secret)
	case "DeleteAccessKey":
		id := form.Get("AccessKeyId")
		delete(u.keys, id)
		u.order = slices.DeleteFunc(u.order, func(k string) bool { return k == id })
		fmt.Fprint(w, `<DeleteAccessKeyResponse/>`)
	case "ListAccessKeys":
		var ms []member
		for _, id := range u.order {
			ms = append(ms, member{UserName: name, AccessKeyID: id})
		}
		out, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"ListAccessKeysResponse"`
			Members []member `xml:"ListAccessKeysResult>AccessKeyMetadata>member"`
		}{Members: ms})
		_, _ = w.Write(out)
	case "ListUsers":
		ms := []member{{UserName: "admin-" + sw.adminKeyIDLocked()}}
		for n := range sw.users {
			ms = append(ms, member{UserName: n})
		}
		out, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"ListUsersResponse"`
			Members []member `xml:"ListUsersResult>Users>member"`
		}{Members: ms})
		_, _ = w.Write(out)
	default:
		sw.xmlError(w, http.StatusBadRequest, "InvalidAction")
	}
}

// adminKeyIDLocked supplies the suffix used in the static admin's ListUsers name (SeaweedFS
// surfaces it as admin-<first 8 chars of the key>).
func (sw *seaweedFake) adminKeyIDLocked() string { return "static" }

// giveScopedKey provisions the project's IAM user and key, matching post-setup state on secured storage.
func (sw *seaweedFake) giveScopedKey(project string) (id, secret string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	name := storage.ScopedUserName(project)
	u := sw.users[name]
	if u == nil {
		u = &fakeIAMUser{keys: map[string]string{}, tags: map[string]string{"rnu-namespace": project}, policy: storage.ScopedPolicy}
		sw.users[name] = u
	}
	sw.nextKey++
	id, secret = fmt.Sprintf("SCOPED%04d", sw.nextKey), fmt.Sprintf("scoped-secret-%d", sw.nextKey)
	u.keys[id] = secret
	u.order = append(u.order, id)
	return id, secret
}

// secretOf returns the secret of an access key ID.
func (sw *seaweedFake) secretOf(id string) string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	for _, u := range sw.users {
		if s, ok := u.keys[id]; ok {
			return s
		}
	}
	return ""
}

// assertScopedKey checks that both of project's Secrets hold the one key
// of the project's SeaweedFS user, never the storage's admin key, and
// returns its ID.
func assertScopedKey(t *testing.T, f *resourceFake, project string) string {
	t.Helper()
	user := storage.ScopedUserName(project)
	ids := f.sw.keysOf(user)
	if len(ids) != 1 {
		t.Fatalf("user %s has keys %v, want one", user, ids)
	}
	if ids[0] == f.sw.adminKeyID() {
		t.Fatalf("%s holds the admin key", project)
	}
	for _, name := range []string{storage.DSPASecretName, storage.ConnectionSecretName} {
		path := secretPath(project, name)
		if got := secretValue(t, f, path, storage.S3AccessKeyField); got != ids[0] {
			t.Errorf("%s: access key %q, want the user's %q", path, got, ids[0])
		}
		if got := secretValue(t, f, path, storage.S3SecretKeyField); got != f.sw.secretOf(ids[0]) {
			t.Errorf("%s: the secret key is not the user's", path)
		}
	}
	return ids[0]
}

// verifySigV4 checks a request's AWS Signature Version 4 with secret, as
// SeaweedFS does, written apart from the tool's signer (s3_iam.go) so a
// signing mistake there fails here: the Authorization header's scope,
// signed headers and signature, the payload hash and the date.
func verifySigV4(r *http.Request, body []byte, id, secret string) error {
	auth := r.Header.Get("Authorization")
	const algo = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, algo) {
		return fmt.Errorf("not SigV4: %q", auth)
	}
	fields := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(auth, algo), ", ") {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	scope := strings.Split(fields["Credential"], "/")
	if len(scope) != 5 || scope[0] != id || scope[2] != "us-east-1" || scope[4] != "aws4_request" || (scope[3] != "s3" && scope[3] != "iam") {
		return fmt.Errorf("bad credential scope %q", fields["Credential"])
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if !strings.HasPrefix(amzDate, scope[1]) {
		return fmt.Errorf("date %q outside scope %q", amzDate, scope[1])
	}
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	if r.Header.Get("X-Amz-Content-Sha256") != payload {
		return fmt.Errorf("payload hash does not match the body")
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	var canonHeaders strings.Builder
	for _, h := range signed {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	// The query, as SigV4 wants it: each key and value URI-encoded (spaces
	// as %20), sorted.
	// The query the way SeaweedFS 4.54 builds it (weed/s3api/auth_signature_v4.go
	// getCanonicalRequest): r.URL.Query(), each key's values sorted,
	// url.Values.Encode() (sorted by key), then "+" as "%20".
	q := r.URL.Query()
	for _, vs := range q {
		sort.Strings(vs)
	}
	pairs := []string{strings.ReplaceAll(q.Encode(), "+", "%20")}
	canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), strings.Join(pairs, "&"), canonHeaders.String(), fields["SignedHeaders"], payload}, "\n")
	creq := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + strings.Join(scope[1:], "/") + "\n" + hex.EncodeToString(creq[:])
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), scope[1])
	k = mac(k, scope[2])
	k = mac(k, scope[3])
	k = mac(k, "aws4_request")
	if want := hex.EncodeToString(mac(k, toSign)); !hmac.Equal([]byte(want), []byte(fields["Signature"])) {
		return fmt.Errorf("signature does not match")
	}
	return nil
}

// untag removes a user's tags; retag points its project tag elsewhere.
func (sw *seaweedFake) untag(user string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.users[user].tags = map[string]string{}
}

func (sw *seaweedFake) retag(user, project string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.users[user].tags["rnu-namespace"] = project
}

// allCalls returns every request recorded; resetCalls forgets them.
func (sw *seaweedFake) allCalls() []string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return slices.Clone(sw.calls)
}

func (sw *seaweedFake) resetCalls() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.calls = nil
}
