package cluster

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// tCNPG is the choice earlier builds always made; the older MaaS tests run
// with it.
var tCNPG = &types.MaaSDatabaseRequest{Kind: "cnpg"}

const (
	tPOCDeploy   = "/apis/apps/v1/namespaces/redhat-ai-gateway-infra/deployments/postgres"
	tPOCService  = "/api/v1/namespaces/redhat-ai-gateway-infra/services/postgres"
	tPOCCreds    = "/api/v1/namespaces/redhat-ai-gateway-infra/secrets/postgres-creds"
	tUserDBPass  = "Sup3r-S3cret-pw"
	tUserDBURL   = "postgresql://maasuser:" + tUserDBPass + "@db.example.com:5432/maas?sslmode=require"
	tForeignSubs = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maassubscriptions/team-a"
)

// devDatabaseRuns makes the fake's Deployment postgres Available on read,
// as its controller would once the pod is ready.
func devDatabaseRuns(mc *modelsCluster) {
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if k.plural == "deployments" && k.name == "postgres" {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "True"}}}
		}
	}
}

func secretURL(t *testing.T, mc *modelsCluster) string {
	t.Helper()
	s := mc.f.get(tMaaSDBConfig)
	if s == nil {
		t.Fatal("no maas-db-config")
	}
	raw, _ := ensureMap(s, "data")["DB_CONNECTION_URL"].(string)
	b, _ := base64.StdEncoding.DecodeString(raw)
	return string(b)
}

// leakForms are the ways a secret could show: as it is, base64 (a
// Secret's data), percent-encoded every way a URL holds it, and
// JSON-escaped.
func leakForms(secret string) []string {
	var hex strings.Builder
	for _, c := range []byte(secret) {
		fmt.Fprintf(&hex, "%%%02x", c)
	}
	j, _ := json.Marshal(secret)
	forms := []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), hex.String(), strings.ToUpper(hex.String()),
		url.QueryEscape(secret), url.PathEscape(secret), string(j[1 : len(j)-1])}
	return forms
}

// captureSlog sends the log to a buffer for the rest of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// noLeak fails when the secret shows, in any of its forms, anywhere but in
// maas-db-config and postgres-creds: responses, step events, this tool's
// records and every other object (activity included), and the status.
func noLeak(t *testing.T, mc *modelsCluster, secret string, res *types.OperationResponse, events []UpdateStepEvent) {
	t.Helper()
	out, _ := json.Marshal(struct {
		R *types.OperationResponse
		E []UpdateStepEvent
	}{res, events})
	var status []byte
	if st, err := GetMaaSStatus(mc.c); err == nil {
		status, _ = json.Marshal(st)
	}
	for _, form := range leakForms(secret) {
		if strings.Contains(string(out), form) {
			t.Fatalf("the secret (as %q) is in the response or the steps: %s", form, out)
		}
		if strings.Contains(string(status), form) {
			t.Fatalf("the secret (as %q) is in the status", form)
		}
	}
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	for k, obj := range mc.f.objects {
		if k.name == "maas-db-config" || k.name == "postgres-creds" {
			continue
		}
		raw, _ := json.Marshal(obj)
		for _, form := range append(leakForms(secret), base64.StdEncoding.EncodeToString([]byte(tUserDBURL))) {
			if strings.Contains(string(raw), form) {
				t.Fatalf("the secret (as %q) is in %+v", form, k)
			}
		}
	}
}

// echoedSecrets are the connection URLs and passwords in the bodies the
// fake quoted back: what a refusal could carry.
func echoedSecrets(t *testing.T, mc *modelsCluster) []string {
	t.Helper()
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	var out []string
	for _, body := range mc.f.echoed {
		var obj struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("echoed %q: %v", body, err)
		}
		for _, k := range []string{"DB_CONNECTION_URL", "POSTGRES_PASSWORD"} {
			if v, err := base64.StdEncoding.DecodeString(obj.Data[k]); err == nil && len(v) > 0 {
				out = append(out, string(v))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the fake quoted no secret back: the test checks nothing")
	}
	return out
}

// An API server's refusal can quote the object it refuses: the connection
// URL (base64 in maas-db-config's data) and the development database's
// password (base64 in postgres-creds'). No message carries them, in any
// form, whichever write is refused: a dry run, a create, an apply of an
// existing Secret, in Set up and in Change database.
func TestMaaSDatabase_RefusalsQuotingTheSecretAreRedacted(t *testing.T) {
	const secrets = "/api/v1/namespaces/redhat-ai-gateway-infra/secrets"
	otherURL := "postgresql://other:" + "Tw0-%40th3r" + "@db2.example.com/maas?sslmode=require"
	ext := func(u string) *types.MaaSDatabaseRequest {
		return &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: u}
	}
	dev := &types.MaaSDatabaseRequest{Kind: "development"}
	for _, tc := range []struct {
		name string
		prep *types.MaaSDatabaseRequest
		req  types.MaaSRequest
		echo string
	}{
		{"Set up, yours: the dry run of maas-db-config", nil, types.MaaSRequest{Action: "setup", Database: ext(tUserDBURL)}, "DRYRUN POST " + secrets},
		{"Set up, yours: the write of maas-db-config", nil, types.MaaSRequest{Action: "setup", Database: ext(tUserDBURL)}, "POST " + secrets},
		{"Set up, development: the dry run of postgres-creds", nil, types.MaaSRequest{Action: "setup", Database: dev}, "DRYRUN POST " + secrets},
		{"Set up, development: the write of postgres-creds", nil, types.MaaSRequest{Action: "setup", Database: dev}, "POST " + secrets},
		{"Change database, yours to yours: the dry run of the apply", ext(tUserDBURL), types.MaaSRequest{Action: "change-database", Database: ext(otherURL), ConfirmKeysLost: true}, "DRYRUN PATCH " + secrets + "/maas-db-config"},
		{"Change database, yours to yours: the apply", ext(tUserDBURL), types.MaaSRequest{Action: "change-database", Database: ext(otherURL), ConfirmKeysLost: true}, "PATCH " + secrets + "/maas-db-config"},
		{"Change database, yours to development: the write of postgres-creds", ext(tUserDBURL), types.MaaSRequest{Action: "change-database", Database: dev, ConfirmKeysLost: true}, "POST " + secrets},
		{"Change database, yours to development: the apply of maas-db-config", ext(tUserDBURL), types.MaaSRequest{Action: "change-database", Database: dev, ConfirmKeysLost: true}, "PATCH " + secrets + "/maas-db-config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			mc := newMaaSCluster(t)
			devDatabaseRuns(mc)
			if tc.prep != nil {
				if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tc.prep}); !res.Success {
					t.Fatalf("prep: %+v", res)
				}
			}
			mc.f.failEcho[tc.echo] = 422
			res, events := runMaaSOp(t, mc.c, tc.req)
			if res.Success {
				t.Fatalf("succeeded although %s was refused: %+v", tc.echo, res)
			}
			for _, s := range echoedSecrets(t, mc) {
				noLeak(t, mc, s, res, events)
				if u, err := url.Parse(s); err == nil && u.User != nil {
					p, _ := u.User.Password()
					noLeak(t, mc, p, res, events)
				}
				for _, form := range leakForms(s) {
					if strings.Contains(logs.String(), form) {
						t.Fatalf("the secret (as %q) is in the log", form)
					}
				}
			}
			if !strings.Contains(res.Message, "[redacted]") {
				t.Errorf("the refusal is not shown, redacted: %q", res.Message)
			}
		})
	}
}

func TestValidateMaaSConnectionURL(t *testing.T) {
	for raw, want := range map[string]string{
		tUserDBURL:                                         "",
		"postgres://u@db:5432/maas":                        "",
		"postgresql://u:p%40ss@db/maas":                    "",
		"mysql://u:p@db/maas":                              "must begin with postgresql:// or postgres://",
		"postgresql://u:p@/maas":                           "names no host",
		"postgresql://db:5432/maas":                        "names no user",
		"postgresql://u:p@db:70000/maas":                   "port must be 1 to 65535",
		"postgresql://u:p@db:5432/":                        "names no database",
		"postgresql://u:p@db/maas/x":                       "names no database",
		"postgresql://u:p@ss@db/maas":                      "",
		"postgresql://u:p ss@db/maas":                      "space or a control character",
		"postgresql://u:p#ss@db/maas":                      "has a #",
		"postgresql://u:p@db/" + strings.Repeat("a", 2100): "longer than 2048",
	} {
		got := validateMaaSConnectionURL(raw)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
		if got != "" && (strings.Contains(got, "p@ss") || strings.Contains(got, "p ss") || strings.Contains(got, "p#ss") || strings.Contains(got, "db:70000")) {
			t.Errorf("%q: the message quotes the URL: %q", raw, got)
		}
	}
	for d, want := range map[types.MaaSDatabaseRequest]string{
		{Kind: "external"}: "enter your PostgreSQL's connection URL",
		{Kind: "development", ConnectionURL: tUserDBURL}: "only Your PostgreSQL takes a connection URL",
		{Kind: "rds"}:  "the database must be external, development or cnpg",
		{Kind: "cnpg"}: "",
	} {
		if got := validateMaaSDatabaseRequest(d); got != want {
			t.Errorf("%s: %q, want %q", d.Kind, got, want)
		}
	}
	if got := redactURL("rejected "+tUserDBURL+" and "+tUserDBPass, tUserDBURL); strings.Contains(got, tUserDBPass) {
		t.Errorf("redactURL left the password: %q", got)
	}
}

// redactURL covers every form a secret can take in a message, any
// password however short, and overlapping finds as one.
func TestRedactURL_EveryForm(t *testing.T) {
	typed := "postgresql://u:p%40ss@db/maas" // the password is p@ss, typed percent-encoded
	for _, leak := range []string{typed, base64.StdEncoding.EncodeToString([]byte(typed)), "u:p%40ss", "p%40ss", "p@ss", "p%40ss", "%70%40%73%73", "%70%40%73%73"} {
		if got := redactURL("refused: "+leak+" here", typed); strings.Contains(got, "p@ss") || strings.Contains(got, "40ss") || got != "refused: [redacted] here" {
			t.Errorf("%q: %q", leak, got)
		}
	}
	dev := maasPOCConnectionURL("pw")
	for _, leak := range []string{dev, base64.StdEncoding.EncodeToString([]byte(dev)), "%70%77", "pw", base64.StdEncoding.EncodeToString([]byte("pw"))} {
		if got := (&maasDBPlan{password: "pw"}).redact("x " + leak + " y"); got != "x [redacted] y" {
			t.Errorf("dev %q: %q", leak, got)
		}
	}
	if got := redactURL("abcab", "postgresql://u:ab@h/d"); got != "[redacted]c[redacted]" {
		t.Errorf("a 2-character password: %q", got)
	}
	if got := redactURL("nothing here", ""); got != "nothing here" {
		t.Errorf("no secret: %q", got)
	}
}

// The development database is setup-database.sh's objects, field for field.
func TestMaaSPOCObjects_AreTheDocsScript(t *testing.T) {
	d := maasPOCDeploymentObject("ns", "img", "uid-7")
	raw, _ := json.Marshal(d)
	for _, want := range []string{`"replicas":1`, `"matchLabels":{"app":"postgres"}`, `"template":{"metadata":{"annotations":{"rhoai-nightly-updater/postgres-creds-uid":"uid-7"},"labels":{"app":"postgres"}}`,
		`{"name":"POSTGRESQL_USER","valueFrom":{"secretKeyRef":{"key":"POSTGRES_USER","name":"postgres-creds"}}}`,
		`{"name":"POSTGRESQL_PASSWORD","valueFrom":{"secretKeyRef":{"key":"POSTGRES_PASSWORD","name":"postgres-creds"}}}`,
		`{"name":"POSTGRESQL_DATABASE","valueFrom":{"secretKeyRef":{"key":"POSTGRES_DB","name":"postgres-creds"}}}`,
		`"ports":[{"containerPort":5432}]`, `"mountPath":"/var/lib/pgsql/data"`, `"emptyDir":{}`,
		`"limits":{"cpu":"500m","memory":"512Mi"}`, `"requests":{"cpu":"100m","memory":"256Mi"}`,
		`"readinessProbe":{"exec":{"command":["/usr/libexec/check-container"]},"initialDelaySeconds":5,"periodSeconds":5}`,
		`"purpose":"poc"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("Deployment lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "securityContext") {
		t.Error("the script sets no security context")
	}
	svc, _ := json.Marshal(maasPOCServiceObject("ns"))
	if !strings.Contains(string(svc), `"ports":[{"port":5432,"targetPort":5432}]`) || !strings.Contains(string(svc), `"selector":{"app":"postgres"}`) {
		t.Errorf("Service: %s", svc)
	}
	if got := maasPOCConnectionURL("aB/"); got != "postgresql://maas:%61%42%2f@postgres:5432/maas?sslmode=disable" {
		t.Errorf("URL %q: every byte percent-encoded, as od -tx1 does", got)
	}
	for range 20 {
		p, err := newMaaSPOCPassword()
		if err != nil || len(p) != 32 || strings.ContainsAny(p, "/+=") {
			t.Fatalf("password %q %v", p, err)
		}
	}
}

func TestMaaSSetup_AsksForTheDatabaseFirst(t *testing.T) {
	mc := newMaaSCluster(t)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if st.Database == nil || !st.Database.Needed || st.Database.Default != "development" || len(st.Database.Choices) != 3 {
		t.Fatalf("database: %+v", st.Database)
	}
	if strings.Contains(strings.Join(st.SetupPlan, "\n"), "CloudNativePG") {
		t.Errorf("the plan installs CloudNativePG without being asked: %v", st.SetupPlan)
	}
	for _, p := range st.Parts {
		if p.ID == "cnpg" {
			t.Error("the CloudNativePG part shows before anyone chose it")
		}
	}
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"})
	if res.Success || res.ErrorCode != "validation" || !strings.Contains(res.Message, "Choose where MaaS keeps its API keys") {
		t.Fatalf("setup without a choice: %+v", res)
	}
	if m := mc.f.mutations(); len(m) > 0 {
		t.Fatalf("changed: %v", m)
	}
}

func TestMaaSDatabase_DevelopmentSetupStatusSwitchAndRemove(t *testing.T) {
	mc := newMaaSCluster(t)
	devDatabaseRuns(mc)
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := finalSteps(events); !strings.Contains(got, "cnpg=success database=success") {
		t.Fatalf("steps = %s", got)
	}
	if mc.f.has(tMaaSDB) {
		t.Fatal("a CloudNativePG cluster was created")
	}
	for _, p := range []string{tPOCDeploy, tPOCService, tPOCCreds} {
		obj := mc.f.get(p)
		if obj == nil || !toolOwnedFake(obj) {
			t.Fatalf("%s: %v", p, obj)
		}
	}
	creds := ensureMap(mc.f.get(tPOCCreds), "data")
	pw, _ := base64.StdEncoding.DecodeString(creds["POSTGRES_PASSWORD"].(string))
	if got := secretURL(t, mc); got != maasPOCConnectionURL(string(pw)) {
		t.Fatalf("maas-db-config %q", got)
	}
	noLeak(t, mc, string(pw), res, events)
	if rec := readRecord(t, mc.c); rec.Database != "development" {
		t.Fatalf("record %+v", rec)
	}
	st, _ := GetMaaSStatus(mc.c)
	db := maasPart(t, st, "database")
	if st.Database.Current != "development" || db.State != "ready" || !strings.Contains(db.Message, "Ephemeral") || len(db.Actions) != 1 || db.Actions[0].ID != "change-database" {
		t.Fatalf("status %+v %+v", st.Database, db)
	}

	// Set up again keeps the password (the script reuses postgres-creds).
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	creds2 := ensureMap(mc.f.get(tPOCCreds), "data")
	if creds2["POSTGRES_PASSWORD"] != creds["POSTGRES_PASSWORD"] {
		t.Fatal("the password changed")
	}
	// Choosing another kind in Set up is refused: Change database does it.
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); res.Success || res.ErrorCode != "conflict" {
		t.Fatalf("setup with another kind: %+v", res)
	}

	// Change database: refused while others use MaaS, without changes.
	mc.f.putJSON(tForeignSubs, `{}`)
	mc.f.requests = nil
	change := types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true}
	if res, _ := runMaaSOp(t, mc.c, change); res.Success || res.ErrorCode != "in_use" {
		t.Fatalf("change while others use MaaS: %+v", res)
	}
	if m := mc.f.mutations(); len(m) > 0 {
		t.Fatalf("changed: %v", m)
	}
	deleteFakeObject(mc, tForeignSubs)
	res, events = runMaaSOp(t, mc.c, change)
	if !res.Success {
		t.Fatalf("change: %+v", res)
	}
	if got := finalSteps(events); got != "check=success cnpg=success database=success cleanup=success" {
		t.Fatalf("steps = %s", got)
	}
	if !mc.f.has(tMaaSDB) || mc.f.has(tPOCDeploy) || mc.f.has(tPOCService) || mc.f.has(tPOCCreds) {
		t.Fatal("the switch did not make the new database and delete the old one")
	}
	if got := secretURL(t, mc); !strings.Contains(got, "@maas-db-rw.redhat-ai-gateway-infra.svc.cluster.local:5432/maas?sslmode=require") {
		t.Fatalf("maas-db-config %q", got)
	}
	if rec := readRecord(t, mc.c); rec.Database != "cnpg" {
		t.Fatalf("record %+v", rec)
	}

	// Remove deletes the CloudNativePG cluster and the Secret, keeps the operator.
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || mc.f.has(tMaaSDB) || mc.f.has(tMaaSDBConfig) {
		t.Fatalf("teardown: %+v", res)
	}
	if rec := readRecord(t, mc.c); rec.Database != "" || rec.DBNamespace != "" {
		t.Fatalf("record %+v", rec)
	}
}

func TestMaaSDatabase_ExternalNeverShowsTheURLAndRemoveKeepsTheDatabase(t *testing.T) {
	mc := newMaaSCluster(t)
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	if got := secretURL(t, mc); got != tUserDBURL {
		t.Fatalf("maas-db-config %q", got)
	}
	if labels := ensureMap(ensureMap(mc.f.get(tMaaSDBConfig), "metadata"), "labels"); labels["app"] != "maas-api" {
		t.Fatalf("labels %v", labels)
	}
	if mc.f.has(tMaaSDB) || mc.f.has(tPOCDeploy) {
		t.Fatal("a database was created")
	}
	noLeak(t, mc, tUserDBPass, res, events)
	st, _ := GetMaaSStatus(mc.c)
	if st.Database.Current != "external" || maasPart(t, st, "database").State != "ready" {
		t.Fatalf("status %+v", st.Database)
	}
	// A dry-run refusal that quotes the URL is redacted.
	mc2 := newMaaSCluster(t)
	mc2.f.failDryRun["POST /api/v1/namespaces/redhat-ai-gateway-infra/secrets"] = 422
	res2, ev2 := runMaaSOp(t, mc2.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}})
	if res2.Success || len(mc2.f.mutations()) > 0 {
		t.Fatalf("dry-run refusal: %+v %v", res2, mc2.f.mutations())
	}
	noLeak(t, mc2, tUserDBPass, res2, ev2)

	res, events = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || mc.f.has(tMaaSDBConfig) {
		t.Fatalf("teardown: %+v", res)
	}
	noLeak(t, mc, tUserDBPass, res, events)
}

func TestMaaSDatabase_AdoptedAndForeignObjectsAreLeftAlone(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON(tMaaSDBConfig, `{"data":{"DB_CONNECTION_URL":"eA=="}}`)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup with someone's Secret needs no choice: %+v", res)
	}
	st, _ := GetMaaSStatus(mc.c)
	if st.Database.Current != "adopted" || st.Database.Needed || len(maasPart(t, st, "database").Actions) > 0 {
		t.Fatalf("status %+v", st.Database)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("change of an adopted database: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"}); !res.Success || !mc.f.has(tMaaSDBConfig) {
		t.Fatalf("teardown deleted someone's Secret: %+v", res)
	}

	// A Deployment postgres someone made (the docs' script): the
	// development option is refused, nothing changed.
	mc = newMaaSCluster(t)
	mc.f.putJSON(tPOCDeploy, `{}`)
	st, _ = GetMaaSStatus(mc.c)
	if st.Database.Choices[1].BlockedReason == "" {
		t.Fatal("the development choice is offered over someone's Deployment")
	}
	mc.f.requests = nil
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}); res.Success || res.ErrorCode != "not_managed" || len(mc.f.mutations()) > 0 {
		t.Fatalf("setup over someone's Deployment: %+v %v", res, mc.f.mutations())
	}
}

// credsRequests are the requests that name postgres-creds, other than its
// delete (its create goes to the collection): a read or a change.
func credsRequests(mc *modelsCluster) []string {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	var out []string
	for _, r := range mc.f.requests {
		if strings.HasSuffix(r, tPOCCreds) && !strings.HasPrefix(r, "DELETE ") {
			out = append(out, r)
		}
	}
	return out
}

func podCredsAnnotation(mc *modelsCluster) string {
	tmpl := ensureMap(ensureMap(ensureMap(mc.f.get(tPOCDeploy), "spec"), "template"), "metadata")
	v, _ := ensureMap(tmpl, "annotations")[maasPOCCredsAnnotation].(string)
	return v
}

// postgres-creds is only created and deleted, never read or changed (the
// template grants no get or patch on it): this tool knows it by the UID it
// recorded, takes the password from its own maas-db-config, and deletes
// only the object with that UID.
func TestMaaSDatabase_PostgresCredsIsOnlyCreatedAndDeletedByUID(t *testing.T) {
	mc := newMaaSCluster(t)
	devDatabaseRuns(mc)
	dev := types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}
	if res, _ := runMaaSOp(t, mc.c, dev); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	uid := ensureMap(mc.f.get(tPOCCreds), "metadata")["uid"].(string)
	if rec := readRecord(t, mc.c); rec.POCCredsUID != uid {
		t.Fatalf("record %+v, Secret UID %s", rec, uid)
	}
	if got := podCredsAnnotation(mc); got != uid {
		t.Fatalf("the pod names postgres-creds %q, want %q", got, uid)
	}
	// Set up again: the password comes from maas-db-config; postgres-creds
	// is not touched.
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	if got := ensureMap(mc.f.get(tPOCCreds), "metadata")["uid"]; got != uid {
		t.Fatal("postgres-creds was created again although its password was known")
	}
	// Without maas-db-config there is no password to keep: a new one, the
	// recorded Secret deleted by its UID and created again, the pod rolled.
	deleteFakeObject(mc, tMaaSDBConfig)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup without maas-db-config: %+v", res)
	}
	uid2 := ensureMap(mc.f.get(tPOCCreds), "metadata")["uid"].(string)
	creds := ensureMap(mc.f.get(tPOCCreds), "data")
	pw, _ := base64.StdEncoding.DecodeString(creds["POSTGRES_PASSWORD"].(string))
	if uid2 == uid || readRecord(t, mc.c).POCCredsUID != uid2 || podCredsAnnotation(mc) != uid2 || secretURL(t, mc) != maasPOCConnectionURL(string(pw)) {
		t.Fatalf("recreate: uid %s -> %s, record %+v", uid, uid2, readRecord(t, mc.c))
	}
	if r := credsRequests(mc); len(r) != 0 {
		t.Fatalf("postgres-creds was read or changed: %v", r)
	}
	// Someone replaces it: Remove keeps theirs (UID precondition) and says so.
	deleteFakeObject(mc, tPOCCreds)
	mc.f.putJSON(tPOCCreds, `{"data":{"POSTGRES_PASSWORD":"dGhlaXJz"}}`)
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || !mc.f.has(tPOCCreds) || mc.f.has(tPOCDeploy) {
		t.Fatalf("teardown: %+v", res)
	}
	if !strings.Contains(strings.Join(res.Logs, "\n"), "was replaced by one this tool did not create") {
		t.Errorf("logs %v", res.Logs)
	}
	if rec := readRecord(t, mc.c); rec.POCCredsUID != "" {
		t.Fatalf("record %+v", rec)
	}
	for _, r := range credsRequests(mc) {
		if strings.HasPrefix(r, "GET ") || strings.Contains(r, "PATCH ") {
			t.Fatalf("postgres-creds was read or changed: %s", r)
		}
	}
}

// A postgres-creds this tool did not record is someone else's: the
// development choice is refused before any change (its create's dry run
// answers 409), and Remove never deletes one without a record.
func TestMaaSDatabase_UnrecordedPostgresCredsIsForeign(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON(tPOCCreds, `{}`)
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}})
	if res.Success || res.ErrorCode != "not_managed" || !strings.Contains(res.Message, "postgres-creds") || len(mc.f.mutations()) > 0 {
		t.Fatalf("setup over someone's postgres-creds: %+v %v", res, mc.f.mutations())
	}
	// A development database of this tool's with no record of its Secret
	// (an earlier run's record lost): Remove keeps the Secret, saying why.
	mc = newMaaSCluster(t)
	devDatabaseRuns(mc)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	rec := readRecord(t, mc.c)
	rec.POCCredsUID = ""
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success || !mc.f.has(tPOCCreds) || !strings.Contains(strings.Join(res.Logs, "\n"), "no record of creating it") {
		t.Fatalf("teardown without a record: %+v", res)
	}
}

// The development database that never becomes available stops Set up
// after MaaSDevDatabaseTimeout (the injected clock: no real wait), with
// what it made recorded; Set up again finishes it once the pod runs.
func TestMaaSDatabase_DevelopmentNotAvailableTimesOut(t *testing.T) {
	mc := newMaaSCluster(t)
	plain := mc.f.onGet
	dev := types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}
	res, events := runMaaSOp(t, mc.c, dev)
	if res.Success || res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "not available after "+clockText(MaaSDevDatabaseTimeout)) {
		t.Fatalf("setup with a pod that never runs: %+v", res)
	}
	if got := finalSteps(events); !strings.HasSuffix(got, "database=failed") {
		t.Fatalf("steps %s", got)
	}
	if rec := readRecord(t, mc.c); rec.Database != "development" || rec.POCCredsUID == "" || mc.f.has(tMaaSDBConfig) {
		t.Fatalf("record %+v; maas-db-config written before the database runs: %v", rec, mc.f.has(tMaaSDBConfig))
	}
	st, _ := GetMaaSStatus(mc.c)
	if db := maasPart(t, st, "database"); db.State != "missing" || !strings.Contains(db.Message, "maas-db-config") {
		t.Fatalf("status %+v", db)
	}
	mc.f.onGet = plain
	devDatabaseRuns(mc)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success || !mc.f.has(tMaaSDBConfig) {
		t.Fatalf("setup again: %+v", res)
	}
}

// maasPOCRestartWarning: a development pod made after maas-api started
// means its keys and tables are gone; one made before is fine; anything it
// cannot read is no warning.
func TestMaaSPOCRestartWarning(t *testing.T) {
	const pods = "/api/v1/namespaces/redhat-ai-gateway-infra/pods/"
	for _, tc := range []struct {
		name, dbCreated, apiStarted string
		want                        bool
	}{
		{"database replaced after maas-api started", "2026-11-18T10:05:00Z", "2026-11-18T10:00:00Z", true},
		{"database older than maas-api", "2026-11-18T09:00:00Z", "2026-11-18T10:00:00Z", false},
		{"no maas-api running", "2026-11-18T10:05:00Z", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			f.putJSON(pods+"postgres-abc", `{"metadata":{"labels":{"app":"postgres"},"creationTimestamp":"`+tc.dbCreated+`"}}`)
			running := `{}`
			if tc.apiStarted != "" {
				running = `{"running":{"startedAt":"` + tc.apiStarted + `"}}`
			}
			f.putJSON(pods+"maas-api-xyz", `{"status":{"containerStatuses":[{"state":`+running+`}]}}`)
			got := maasPOCRestartWarning(c, "redhat-ai-gateway-infra")
			if (got != "") != tc.want {
				t.Fatalf("warning %q, want one: %v", got, tc.want)
			}
			if tc.want && (!strings.Contains(got, "2026-11-18T10:05:00Z") || !strings.Contains(got, "oc rollout restart deployment/maas-api -n redhat-ai-gateway-infra")) {
				t.Errorf("warning %q", got)
			}
		})
	}
	f, c := newResourceFake(t)
	f.fail["GET /api/v1/namespaces/redhat-ai-gateway-infra/pods"] = 403
	if got := maasPOCRestartWarning(c, "redhat-ai-gateway-infra"); got != "" {
		t.Errorf("unreadable pods: %q", got)
	}
}

// Change database deletes only what this tool made of the database before:
// a Service postgres someone put in its place and a postgres-creds with
// another UID are kept, and the log says so.
func TestMaaSDatabase_ChangeKeepsForeignObjectsOfTheDatabaseBefore(t *testing.T) {
	mc := newMaaSCluster(t)
	devDatabaseRuns(mc)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	// Status reads a foreign Service as a problem, so the swap happens
	// between the change's check and its cleanup.
	swapped := false
	mc.f.beforeServe = func(method, path string) {
		if !swapped && method == "PATCH" && strings.HasSuffix(path, "/maas-db-config") {
			swapped = true
			deleteFakeObject(mc, tPOCService)
			mc.f.putJSON(tPOCService, `{"metadata":{"labels":{"app":"theirs"}}}`)
			deleteFakeObject(mc, tPOCCreds)
			mc.f.putJSON(tPOCCreds, `{}`)
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true})
	if !res.Success || !swapped {
		t.Fatalf("change: %+v (swapped %v)", res, swapped)
	}
	if mc.f.has(tPOCDeploy) || !mc.f.has(tPOCService) || !mc.f.has(tPOCCreds) {
		t.Fatal("the change deleted an object this tool did not make, or kept its own Deployment")
	}
	logs := strings.Join(res.Logs, "\n")
	if !strings.Contains(logs, "Kept Service redhat-ai-gateway-infra/postgres: not created by this tool") || !strings.Contains(logs, "was replaced by one this tool did not create") {
		t.Errorf("logs %s", logs)
	}
}

// A Change database that stops after the new database is in place leaves
// two databases; Remove deletes both (each object this tool made).
func TestMaaSDatabase_ChangeStoppedPartWayThenRemoveCleansBoth(t *testing.T) {
	mc := newMaaSCluster(t)
	devDatabaseRuns(mc)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.fail["DELETE "+tPOCDeploy] = 500
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true})
	if res.Success || !strings.HasSuffix(finalSteps(events), "cleanup=failed") {
		t.Fatalf("change: %+v %s", res, finalSteps(events))
	}
	if !mc.f.has(tMaaSDB) || !mc.f.has(tPOCDeploy) || !mc.f.has(tPOCCreds) {
		t.Fatal("expected both databases after the stopped change")
	}
	if rec := readRecord(t, mc.c); rec.Database != "cnpg" || rec.POCCredsUID == "" {
		t.Fatalf("record %+v", rec)
	}
	if got := secretURL(t, mc); !strings.Contains(got, "maas-db-rw") {
		t.Fatalf("maas-db-config names %q, not the new database", got)
	}
	delete(mc.f.fail, "DELETE "+tPOCDeploy)
	res, _ = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "teardown"})
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	for _, p := range []string{tMaaSDB, tMaaSDBConfig, tPOCDeploy, tPOCService, tPOCCreds} {
		if mc.f.has(p) {
			t.Errorf("%s is left", p)
		}
	}
	if rec := readRecord(t, mc.c); rec.Database != "" || rec.POCCredsUID != "" {
		t.Fatalf("record %+v", rec)
	}
}

// Set up never changes your PostgreSQL's URL in place: a new URL is refused
// (it would be ignored), and Change database does it.
func TestMaaSDatabase_SetUpRefusesANewURLForYours(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.requests = nil
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: "postgresql://u:n3w-pw@db2/maas"}})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "Change database") || len(mc.f.mutations()) > 0 {
		t.Fatalf("setup with a new URL: %+v %v", res, mc.f.mutations())
	}
	noLeak(t, mc, "n3w-pw", res, events)
	if got := secretURL(t, mc); got != tUserDBURL {
		t.Fatal("the URL changed")
	}
}

// Your PostgreSQL is recorded only once its Secret is written: a refused
// write leaves no record, so the status asks again instead of calling the
// Secret lost. A Secret of this tool's with no record (the record write
// failed after it) reads as yours, not as an earlier build's CloudNativePG.
func TestMaaSDatabase_YoursIsRecordedAfterItsSecret(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.fail["POST /api/v1/namespaces/redhat-ai-gateway-infra/secrets"] = 500
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}}); res.Success {
		t.Fatalf("setup with a failing write: %+v", res)
	}
	if rec := readRecord(t, mc.c); rec.Database != "" {
		t.Fatalf("recorded before the Secret: %+v", rec)
	}
	st, _ := GetMaaSStatus(mc.c)
	if db := maasPart(t, st, "database"); st.Database.Current != "" || !st.Database.Needed || strings.Contains(db.Message, "is gone") {
		t.Fatalf("status %+v %+v", st.Database, db)
	}
	delete(mc.f.fail, "POST /api/v1/namespaces/redhat-ai-gateway-infra/secrets")
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
	rec := readRecord(t, mc.c)
	if rec.Database != "external" {
		t.Fatalf("record %+v", rec)
	}
	rec.Database = ""
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	if st, _ := GetMaaSStatus(mc.c); st.Database.Current != "external" {
		t.Fatalf("an unrecorded Secret of this tool's reads as %q", st.Database.Current)
	}
}

// Change database runs only from a database that runs: the check refuses
// before any change.
func TestMaaSDatabase_ChangeRefusesADatabaseThatDoesNotRun(t *testing.T) {
	mc := newMaaSCluster(t)
	plain := mc.f.onGet
	devDatabaseRuns(mc)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "development"}}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	mc.f.onGet = plain // the pod is gone: not Available
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true})
	if res.Success || res.ErrorCode != "not_ready" || len(mc.f.mutations()) > 0 {
		t.Fatalf("change from a database that does not run: %+v %v", res, mc.f.mutations())
	}
}

// A CloudNativePG database an earlier build made (no kind recorded) reads
// as this tool's CloudNativePG one.
func TestMaaSDatabase_EarlierBuildsCNPGIsAdoptedAsCNPG(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	rec := readRecord(t, mc.c)
	rec.Database = ""
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	st, _ := GetMaaSStatus(mc.c)
	if st.Database.Current != "cnpg" || maasPart(t, st, "cnpg").State != "ready" {
		t.Fatalf("status %+v", st.Database)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup"}); !res.Success {
		t.Fatalf("setup again: %+v", res)
	}
}

func toolOwnedFake(obj map[string]any) bool {
	raw, _ := json.Marshal(obj["metadata"])
	var meta objectMeta
	_ = json.Unmarshal(raw, &meta)
	return toolOwned(meta)
}
