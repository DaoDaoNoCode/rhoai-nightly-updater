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
	mc.f.onGet = func(k fakeKey, obj map[string]interface{}) {
		prev(k, obj)
		if k.plural == "deployments" && k.name == "postgres" {
			obj["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Available", "status": "True"}}}
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
	d := maasPOCDeploymentObject("ns", "img")
	raw, _ := json.Marshal(d)
	for _, want := range []string{`"replicas":1`, `"matchLabels":{"app":"postgres"}`, `"template":{"metadata":{"labels":{"app":"postgres"}}`,
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
	for i := 0; i < 20; i++ {
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

func toolOwnedFake(obj map[string]interface{}) bool {
	raw, _ := json.Marshal(obj["metadata"])
	var meta objectMeta
	_ = json.Unmarshal(raw, &meta)
	return toolOwned(meta)
}
