package cluster

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The database maas-api keeps its API keys in. The MaaS docs leave it to
// the customer (install/maas-setup.md "Database Setup", l.14-38: Secret
// maas-db-config with key DB_CONNECTION_URL in the infrastructure
// namespace; l.441: "PostgreSQL is customer-provided: bundled POC Postgres
// from scripts/setup-database.sh, shared in-cluster instances (any
// namespace or labels), and external databases (for example RDS) are all
// valid"; concepts/architecture.md: "A PostgreSQL database is required and
// is not included"). So Set up asks, and offers:
//
//   - "external": the user's PostgreSQL. Only Secret maas-db-config is
//     written, with the URL the user gives; the URL holds a password and is
//     never returned, logged or recorded (redactURL on every message that
//     could carry it).
//   - "development": scripts/setup-database.sh as the docs' Development
//     note runs it (maas-setup.md l.86-103; the script at
//     opendatahub-io/models-as-a-service 08cf031, 2026-11-18): Secret
//     postgres-creds, Deployment postgres (one pod, emptyDir: "Data WILL
//     be lost on pod restart") and Service postgres, then maas-db-config
//     with sslmode=disable. The default: the documented path, nothing else
//     installed, and this tool's clusters are short-lived test clusters.
//   - "cnpg": a one-instance CloudNativePG cluster with a volume. Not in
//     the MaaS or RHOAI docs: this tool's own option (it installs the
//     CloudNativePG operator from certified-operators), and what earlier
//     builds always did.
//
// A maas-db-config this tool did not write is adopted as it is ("adopted").
// RHOAI 3.6's databaseservice is not offered: MaaS has no claim on it.

const (
	maasDBExternal    = "external"
	maasDBDevelopment = "development"
	maasDBCNPG        = "cnpg"
	maasDBAdopted     = "adopted"
	maasDBDefault     = maasDBDevelopment

	maasPOCName          = "postgres"
	maasPOCCreds         = "postgres-creds"
	maasPOCUser          = "maas"
	maasPOCDatabase      = "maas"
	maasPOCImageKey      = "postgresql_16_image"
	maasPOCImageFallback = "registry.redhat.io/rhel9/postgresql-16:latest"
	maasMaxURLBytes      = 2048
)

// MaaSDevDatabaseTimeout bounds the wait for the development database
// (setup-database.sh: kubectl wait --timeout=120s).
var MaaSDevDatabaseTimeout = 2 * time.Minute

func maasDBKindText(kind string) string {
	switch kind {
	case maasDBExternal:
		return "your PostgreSQL"
	case maasDBDevelopment:
		return "the MaaS docs' development database"
	case maasDBCNPG:
		return "the persistent development database on CloudNativePG"
	case maasDBAdopted:
		return "the database maas-db-config names (not written by this tool)"
	}
	return "no database"
}

// --- the request --------------------------------------------------------------------

// validateMaaSDatabaseRequest returns why d cannot be used, or "". Its
// messages never quote the URL.
func validateMaaSDatabaseRequest(d types.MaaSDatabaseRequest) string {
	switch d.Kind {
	case maasDBExternal:
		if d.ConnectionURL == "" {
			return "enter your PostgreSQL's connection URL"
		}
		return validateMaaSConnectionURL(d.ConnectionURL)
	case maasDBDevelopment, maasDBCNPG:
		if d.ConnectionURL != "" {
			return "only Your PostgreSQL takes a connection URL"
		}
		return ""
	}
	return "the database must be external, development or cnpg"
}

// validateMaaSConnectionURL checks DB_CONNECTION_URL's shape
// (maas-setup.md "Connection string format":
// postgresql://USERNAME:PASSWORD@HOSTNAME:PORT/DATABASE?sslmode=require;
// "the URL must use postgresql:// or postgres://"). It never quotes the URL
// or url.Parse's error, which does.
func validateMaaSConnectionURL(raw string) string {
	const format = " (postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require; percent-encode special characters in the password)"
	if len(raw) > maasMaxURLBytes {
		return fmt.Sprintf("the connection URL is longer than %d characters", maasMaxURLBytes)
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "the connection URL has a space or a control character" + format
		}
	}
	if strings.Contains(raw, "#") {
		return "the connection URL has a # (percent-encode it in the password as %23)"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "the connection URL cannot be read" + format
	case u.Scheme != "postgresql" && u.Scheme != "postgres":
		return "the connection URL must begin with postgresql:// or postgres://" + format
	case u.Opaque != "" || u.Hostname() == "":
		return "the connection URL names no host" + format
	case u.User == nil || u.User.Username() == "":
		return "the connection URL names no user" + format
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "the connection URL's port must be 1 to 65535"
		}
	}
	if db := strings.TrimPrefix(u.Path, "/"); db == "" || strings.Contains(db, "/") {
		return "the connection URL names no database" + format
	}
	return ""
}

// secretForms are the ways a connection URL and the passwords (its own,
// and any given) can show in an API server's message, which may quote the
// object it refuses: as sent (a Secret's data is base64), as typed (the
// URL's raw userinfo and password), percent-encoded every way a URL holds
// a password (the development URL encodes every byte), and JSON-escaped.
func secretForms(urls, passwords []string) []string {
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		j, _ := json.Marshal(s)
		out = append(out, s, base64.StdEncoding.EncodeToString([]byte(s)), string(j[1:len(j)-1]))
	}
	for _, connURL := range urls {
		add(connURL)
		if _, rest, ok := strings.Cut(connURL, "://"); ok {
			authority := rest
			if i := strings.IndexAny(rest, "/?#"); i >= 0 {
				authority = rest[:i]
			}
			if at := strings.LastIndex(authority, "@"); at >= 0 {
				add(authority[:at])
				if _, raw, ok := strings.Cut(authority[:at], ":"); ok {
					add(raw)
				}
			}
		}
		if u, err := url.Parse(connURL); err == nil && u.User != nil {
			if p, ok := u.User.Password(); ok {
				passwords = append(passwords, p)
			}
		}
	}
	for _, p := range passwords {
		if p == "" {
			continue
		}
		var hex strings.Builder
		for _, c := range []byte(p) {
			fmt.Fprintf(&hex, "%%%02x", c)
		}
		for _, f := range []string{p, url.QueryEscape(p), url.PathEscape(p), url.UserPassword("", p).String()[1:], hex.String(), strings.ToUpper(hex.String())} {
			add(f)
		}
	}
	return out
}

// redactURL replaces every form of a connection URL and of the passwords
// in msg (secretForms) with [redacted]: an API server's error could quote
// what was sent. Any non-empty password counts, however short: the
// message may become hard to read, never revealing. Overlapping finds are
// redacted as one.
func redactURL(msg, connURL string, passwords ...string) string {
	return redactSecrets(msg, []string{connURL}, passwords)
}

// redact removes every secret the plan holds from msg: the URL given, and
// the development database's password with its URL.
func (p *maasDBPlan) redact(msg string, more ...string) string {
	urls, passwords := append([]string{p.url}, more...), []string{}
	if p.password != "" {
		urls, passwords = append(urls, maasPOCConnectionURL(p.password)), append(passwords, p.password)
	}
	return redactSecrets(msg, urls, passwords)
}

func redactSecrets(msg string, urls, passwords []string) string {
	covered := make([]bool, len(msg))
	any := false
	for _, f := range secretForms(urls, passwords) {
		for from := 0; from < len(msg); {
			i := strings.Index(msg[from:], f)
			if i < 0 {
				break
			}
			for k := from + i; k < from+i+len(f); k++ {
				covered[k] = true
			}
			any = true
			from += i + 1
		}
	}
	if !any {
		return msg
	}
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		switch {
		case !covered[i]:
			b.WriteByte(msg[i])
		case i == 0 || !covered[i-1]:
			b.WriteString("[redacted]")
		}
	}
	return b.String()
}

// --- the database as it is ------------------------------------------------------------

// maasDBState is the database in namespace ns as read: the connection
// Secret, the CloudNativePG cluster, the development database's objects,
// and which of them is the database (kind).
type maasDBState struct {
	ns   string
	kind string
	// lostSecret: this tool set up "external", and its Secret is gone; Set
	// up asks for the URL again.
	lostSecret   bool
	secret       objectMeta
	secretFound  bool
	cluster      objectMeta
	clusterFound bool
	clusterReady bool
	clusterPhase string
	poc          map[string]objectMeta
}

func readMaaSDBState(c *Client, rec maasRecord, ns string) (maasDBState, error) {
	st := maasDBState{ns: ns, poc: map[string]objectMeta{}}
	var err error
	if st.secret, st.secretFound, err = readObjectMeta(c, maasObjectPath("Secret", ns)); err != nil {
		return st, fmt.Errorf("read Secret %s/%s: %w", ns, maasDBConfigSecret, err)
	}
	if st.cluster, st.clusterFound, st.clusterReady, st.clusterPhase, err = readMaaSDBCluster(c, ns); err != nil && !IsK8sError(err, 404) {
		return st, fmt.Errorf("read %s: %w", maasDesc("Cluster", ns), err)
	}
	for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
		meta, found, err := readObjectMeta(c, maasObjectPath(k, ns))
		if err != nil {
			return st, fmt.Errorf("read %s: %w", maasDesc(k, ns), err)
		}
		if found {
			st.poc[k] = meta
		}
	}
	switch {
	case st.secretFound && !toolOwned(st.secret):
		st.kind = maasDBAdopted
	case rec.Database != "":
		st.kind = rec.Database
	case st.clusterFound && toolOwned(st.cluster):
		// Earlier builds made only CloudNativePG databases, and recorded
		// no kind.
		st.kind = maasDBCNPG
	case st.secretFound && toolOwned(st.poc["PostgresDeployment"]):
		st.kind = maasDBDevelopment
	case st.secretFound:
		// This tool's maas-db-config with no database of its own: the
		// user's, whose record write failed after the Secret was written.
		st.kind = maasDBExternal
	}
	if st.kind == maasDBExternal && !st.secretFound {
		st.kind, st.lostSecret = "", true
	}
	return st, nil
}

// foreignPOC names a development-database object of the script's names
// that this tool did not create, or "".
func (s maasDBState) foreignPOC() string {
	for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
		if meta, ok := s.poc[k]; ok && !toolOwned(meta) {
			return maasDesc(k, s.ns)
		}
	}
	return ""
}

func toolDBKind(kind string) bool {
	return kind == maasDBExternal || kind == maasDBDevelopment || kind == maasDBCNPG
}

// --- the development database (scripts/setup-database.sh) ---------------------------

func maasPOCLabels() map[string]interface{} {
	l := toolLabels()
	l["app"] = "postgres"
	l["purpose"] = "poc"
	return l
}

func maasPOCCredsObject(ns, password string) map[string]interface{} {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": maasPOCCreds, "namespace": ns, "labels": maasPOCLabels()},
		"data":       map[string]interface{}{"POSTGRES_USER": b64(maasPOCUser), "POSTGRES_PASSWORD": b64(password), "POSTGRES_DB": b64(maasPOCDatabase)},
	}
}

// maasPOCDeploymentObject is the script's Deployment postgres, field for
// field (it sets no security context: OpenShift's restricted SCC does).
// The tool's label is on the Deployment only, so the selector and the pod
// labels are the script's.
func maasPOCDeploymentObject(ns, image string) map[string]interface{} {
	env := func(name, key string) map[string]interface{} {
		return map[string]interface{}{"name": name, "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": maasPOCCreds, "key": key}}}
	}
	return map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": maasPOCName, "namespace": ns, "labels": maasPOCLabels()},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "postgres"}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "postgres"}},
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{
						"name":  "postgres",
						"image": image,
						"env": []interface{}{
							env("POSTGRESQL_USER", "POSTGRES_USER"),
							env("POSTGRESQL_PASSWORD", "POSTGRES_PASSWORD"),
							env("POSTGRESQL_DATABASE", "POSTGRES_DB"),
						},
						"ports":        []interface{}{map[string]interface{}{"containerPort": 5432}},
						"volumeMounts": []interface{}{map[string]interface{}{"name": "data", "mountPath": "/var/lib/pgsql/data"}},
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"memory": "256Mi", "cpu": "100m"},
							"limits":   map[string]interface{}{"memory": "512Mi", "cpu": "500m"},
						},
						"readinessProbe": map[string]interface{}{
							"exec":                map[string]interface{}{"command": []interface{}{"/usr/libexec/check-container"}},
							"initialDelaySeconds": 5,
							"periodSeconds":       5,
						},
					}},
					"volumes": []interface{}{map[string]interface{}{"name": "data", "emptyDir": map[string]interface{}{}}},
				},
			},
		},
	}
}

func maasPOCServiceObject(ns string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": maasPOCName, "namespace": ns, "labels": maasPOCLabels()},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"app": "postgres"},
			"ports":    []interface{}{map[string]interface{}{"port": 5432, "targetPort": 5432}},
		},
	}
}

func maasPOCObject(kind, ns, password, image string) map[string]interface{} {
	switch kind {
	case "PostgresSecret":
		return maasPOCCredsObject(ns, password)
	case "PostgresDeployment":
		return maasPOCDeploymentObject(ns, image)
	}
	return maasPOCServiceObject(ns)
}

// maasPOCConnectionURL is the script's DB_CONNECTION_URL: every byte of the
// password percent-encoded (od -An -tx1, lower-case hex), the Service's
// short name (maas-api runs in the same namespace) and sslmode=disable
// (DB_SSLMODE's default: the pod has no TLS).
func maasPOCConnectionURL(password string) string {
	var b strings.Builder
	for _, c := range []byte(password) {
		fmt.Fprintf(&b, "%%%02x", c)
	}
	return "postgresql://" + maasPOCUser + ":" + b.String() + "@" + maasPOCName + ":5432/" + maasPOCDatabase + "?sslmode=disable"
}

// newMaaSPOCPassword is the script's password: openssl rand -base64 32 |
// tr -d '/+=' | cut -c1-32 (drawn again in the rare case fewer than 32
// characters are left).
func newMaaSPOCPassword() (string, error) {
	for {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		s := strings.NewReplacer("/", "", "+", "", "=", "").Replace(base64.StdEncoding.EncodeToString(b))
		if len(s) >= 32 {
			return s[:32], nil
		}
	}
}

// maasPOCPassword keeps the password of a postgres-creds this tool made
// (the script reuses an existing one), else draws one.
func maasPOCPassword(c *Client, st maasDBState) (string, error) {
	if meta, ok := st.poc["PostgresSecret"]; ok && toolOwned(meta) {
		body, _, err := c.get(maasObjectPath("PostgresSecret", st.ns))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", maasDesc("PostgresSecret", st.ns), err)
		}
		var s struct {
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal(body, &s) == nil {
			if p, err := base64.StdEncoding.DecodeString(s.Data["POSTGRES_PASSWORD"]); err == nil && len(p) > 0 {
				return string(p), nil
			}
		}
	}
	return newMaaSPOCPassword()
}

// maasPOCImage is the script's resolve_postgres_image: relatedImages
// postgresql_16_image of the RHOAI operator's CSV, else its fallback
// image. It says which.
func maasPOCImage(c *Client) (string, string) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if err == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name   string            `json:"name"`
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					RelatedImages []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"relatedImages"`
				} `json:"spec"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &list) == nil {
			found := ""
			for _, it := range list.Items {
				if !strings.HasPrefix(it.Metadata.Name, SubName+".") || it.Metadata.Labels["olm.copiedFrom"] != "" {
					continue
				}
				for _, ri := range it.Spec.RelatedImages {
					if ri.Name == maasPOCImageKey && ri.Image != "" && (found == "" || it.Status.Phase == "Succeeded") {
						found = ri.Image
					}
				}
			}
			if found != "" {
				return found, "from the RHOAI operator's CSV (" + maasPOCImageKey + ")"
			}
		}
	}
	return maasPOCImageFallback, "the script's fallback image: the RHOAI operator's CSV names none"
}

// maasPOCRestartWarning: the development database's pod was created after
// maas-api started, so its data (API keys and tables) is gone, and maas-api
// makes its tables only when it starts (models-as-a-service rhoai-3.6
// maas-api/internal/api_keys/db_driver.go: migrations run in
// NewPostgresStoreFromURLs, at startup). Best effort: "" when unreadable.
func maasPOCRestartWarning(c *Client, ns string) string {
	body, _, err := c.get(namespacedPath("v1", "pods", ns, ""))
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Labels            map[string]string `json:"labels"`
				CreationTimestamp string            `json:"creationTimestamp"`
				DeletionTimestamp string            `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				ContainerStatuses []struct {
					State struct {
						Running *struct {
							StartedAt string `json:"startedAt"`
						} `json:"running"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return ""
	}
	var dbCreated, apiStarted time.Time
	for _, p := range list.Items {
		if p.Metadata.DeletionTimestamp != "" {
			continue
		}
		switch {
		case p.Metadata.Labels["app"] == "postgres":
			if t, ok := parseK8sTime(p.Metadata.CreationTimestamp); ok && t.After(dbCreated) {
				dbCreated = t
			}
		case strings.HasPrefix(p.Metadata.Name, "maas-api-"):
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Running == nil {
					continue
				}
				if t, ok := parseK8sTime(cs.State.Running.StartedAt); ok && (apiStarted.IsZero() || t.Before(apiStarted)) {
					apiStarted = t
				}
			}
		}
	}
	if dbCreated.IsZero() || apiStarted.IsZero() || !dbCreated.After(apiStarted) {
		return ""
	}
	return fmt.Sprintf("The development database's pod was replaced at %s, after maas-api started: its data is gone, API keys included, and maas-api makes its tables only when it starts. Restart maas-api (oc rollout restart deployment/maas-api -n %s), then create API keys again.", dbCreated.UTC().Format(time.RFC3339), ns)
}

// --- the status -------------------------------------------------------------------------

// cnpgReadiness is CloudNativePG as the cnpg choice needs it.
type cnpgReadiness struct {
	served  bool
	err     error
	pending bool
	plan    *operatorInstallPlan
	planMsg string
}

func readCNPGReadiness(c *Client, cnpg operatorState, readErr error) cnpgReadiness {
	served, err := groupServed(c, cnpgGroup)
	if err == nil && served && !cnpg.installed() {
		// Its API alone is not an installed operator (CRDs stay after an
		// uninstall).
		served, err = controllerDeployed(c, cnpgController)
	}
	r := cnpgReadiness{served: served, err: err}
	switch {
	case err != nil || served:
	case readErr != nil:
		r.err = readErr
	case cnpg.installed() || cnpg.pending() != nil:
		r.pending = true
	default:
		r.plan, r.planMsg, _ = planOperatorInstall(c, cnpgOperator)
	}
	return r
}

func cnpgInstallPlanText(p *operatorInstallPlan) string {
	return fmt.Sprintf("Install CloudNativePG %s: %sSubscription %s/%s (package %s, channel %s, from certified-operators; in its own namespace, not openshift-operators, where an install can pull an OSSM upgrade that breaks the gateway; it stays after Remove)", p.head, operatorPlanPrefix(p), cnpgOperator.namespace, cnpgOperator.subName, cnpgOperator.pkg, p.channel)
}

// maasDBChoices are the databases Set up (or Change database) can make in
// st.ns now, with what each creates.
func maasDBChoices(c *Client, st maasDBState, cr cnpgReadiness) []types.MaaSDatabaseChoice {
	ns := st.ns
	ext := types.MaaSDatabaseChoice{ID: maasDBExternal, Plan: []string{fmt.Sprintf("Write Secret %s/%s (label app=maas-api) with the connection URL you enter; nothing else is created, and your database is never changed", ns, maasDBConfigSecret)}}
	image, from := maasPOCImage(c)
	dev := types.MaaSDatabaseChoice{ID: maasDBDevelopment, Plan: []string{fmt.Sprintf("Run the MaaS docs' development PostgreSQL (scripts/setup-database.sh) in %s: Secret %s (a generated password), Deployment %s (one pod, image %s, %s; no volume, so its data is lost when the pod restarts) and Service %s, then Secret %s with its address (sslmode=disable, as the script does)", ns, maasPOCCreds, maasPOCName, image, from, maasPOCName, maasDBConfigSecret)}}
	if foreign := st.foreignPOC(); foreign != "" {
		dev.BlockedReason = foreign + " exists and was not created by this tool (the docs' script uses the same names), so this option would take it over. Its owner removes it, or choose another option."
	}
	cn := types.MaaSDatabaseChoice{ID: maasDBCNPG}
	switch {
	case cr.err != nil:
		cn.BlockedReason = "Cannot tell whether CloudNativePG is installed: " + cr.err.Error()
	case cr.served:
	case cr.pending:
		cn.Plan = append(cn.Plan, "Wait for OLM to finish installing CloudNativePG")
	case cr.planMsg != "":
		cn.BlockedReason = "CloudNativePG cannot be installed: " + cr.planMsg
	case cr.plan != nil:
		cn.Plan = append(cn.Plan, cnpgInstallPlanText(cr.plan))
	}
	if st.clusterFound && !toolOwned(st.cluster) {
		cn.BlockedReason = maasDesc("Cluster", ns) + " exists and was not created by this tool, so it is left alone. Choose another option."
	}
	cn.Plan = append(cn.Plan, fmt.Sprintf("Run a one-instance CloudNativePG database %s/%s (1 GiB volume, so keys survive restarts) and write Secret %s/%s with its address (sslmode=require)", ns, maasDBCluster, ns, maasDBConfigSecret))
	return []types.MaaSDatabaseChoice{ext, dev, cn}
}

// maasDBStatus is the database's share of GetMaaSStatus.
type maasDBStatus struct {
	parts    []types.MaaSPart
	db       *types.MaaSDatabase
	plan     []string
	blockers []string
}

// maasDatabaseStatus reports the database (and CloudNativePG while it is
// the database), the choice Set up asks for, and what Set up does about it.
func maasDatabaseStatus(c *Client, rec maasRecord, infra string, cnpg operatorState, cnpgReadErr error) maasDBStatus {
	ns := firstNonEmpty(rec.DBNamespace, infra)
	out := maasDBStatus{db: &types.MaaSDatabase{Namespace: ns, Default: maasDBDefault}}
	part := types.MaaSPart{ID: "database", Title: "Database (PostgreSQL) and maas-db-config", DocURL: maasDocsBase + "install/maas-setup.md#database-setup"}
	done := func() maasDBStatus {
		if rec.InfraNamespace != "" && rec.InfraNamespace == ns {
			part.ManagedByTool = true
		}
		out.parts = append(out.parts, part)
		return out
	}
	_, nsFound, err := readObjectMeta(c, "/api/v1/namespaces/"+ns)
	if err != nil {
		part.State, part.Message = "unknown", fmt.Sprintf("Cannot read namespace %s: %v", ns, err)
		return done()
	}
	st, err := readMaaSDBState(c, rec, ns)
	if err != nil {
		part.State, part.Message = "unknown", "Cannot read the database: "+err.Error()
		return done()
	}
	out.db.Current = st.kind
	if !nsFound {
		if _, ours := maasInfraNamespacePath(ns); !ours {
			part.State, part.Message = "problem", fmt.Sprintf("Namespace %s, where maas-api and its database go, does not exist, and this tool creates only the AI gateway's own infrastructure namespaces. A cluster-admin creates it.", ns)
			out.blockers = append(out.blockers, part.Message)
			return done()
		}
		out.plan = append(out.plan, fmt.Sprintf("Create namespace %s (the AI gateway's infrastructure namespace, where the docs' setup-database.sh puts the database; recorded, and deleted by Remove when nothing else is in it)", ns))
	}
	var cr cnpgReadiness
	if st.kind == "" || toolDBKind(st.kind) {
		cr = readCNPGReadiness(c, cnpg, cnpgReadErr)
		out.db.Choices = maasDBChoices(c, st, cr)
	}
	switch st.kind {
	case "":
		out.db.Needed = true
		part.State = "missing"
		part.Message = "Not set up yet. Set up asks where MaaS keeps its API keys: your PostgreSQL, the MaaS docs' development database (one pod, ephemeral), or a persistent development database on CloudNativePG (this tool's own option, not in the MaaS docs)."
		if st.lostSecret {
			part.Message = fmt.Sprintf("Secret %s/%s, which this tool wrote for your PostgreSQL, is gone. Set up asks for the database again.", ns, maasDBConfigSecret)
		}
	case maasDBAdopted:
		part.State, part.NotManaged = "ready", true
		part.Message = fmt.Sprintf("Secret %s/%s exists and was not written by this tool: maas-api uses the database it names, as it is. This tool never changes or deletes it.", ns, maasDBConfigSecret)
	case maasDBExternal:
		part.State, part.ManagedByTool = "ready", true
		part.Message = fmt.Sprintf("Your PostgreSQL: Secret %s/%s, written by this tool, holds the connection URL you entered (never shown). Remove deletes only that Secret; your database is never touched.", ns, maasDBConfigSecret)
	case maasDBDevelopment:
		part.ManagedByTool = true
		var missing []string
		for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
			if _, ok := st.poc[k]; !ok {
				missing = append(missing, maasDesc(k, ns))
			}
		}
		if !st.secretFound {
			missing = append(missing, maasDesc("Secret", ns))
		}
		switch avail := deploymentAvailable(c, ns, maasPOCName); {
		case st.foreignPOC() != "":
			part.State, part.Message = "problem", st.foreignPOC()+" was replaced by an object this tool did not create; this tool leaves it alone."
		case len(missing) > 0:
			part.State, part.Message = "missing", "Set up finishes the MaaS docs' development database: "+strings.Join(missing, ", ")+"."
			out.plan = append(out.plan, "Finish the MaaS docs' development database: create "+strings.Join(missing, ", "))
		case avail == "True":
			part.State = "ready"
			part.Message = fmt.Sprintf("The MaaS docs' development database %s/%s runs (one pod, no volume). Ephemeral: API keys are lost when its pod restarts, and maas-api then needs a restart to make its tables again.", ns, maasPOCName)
			part.Warning = maasPOCRestartWarning(c, ns)
		case avail == "":
			part.State, part.Message = "unknown", "Cannot read "+maasDesc("PostgresDeployment", ns)
		default:
			part.State, part.Message = "waiting", "The development database is starting"
		}
	case maasDBCNPG:
		out.parts = append(out.parts, cnpgStatusPart(c, rec, cnpg, cr))
		part.ManagedByTool = true
		switch {
		case st.clusterFound && !toolOwned(st.cluster):
			part.State, part.NotManaged = "problem", true
			part.Message = fmt.Sprintf("%s exists and was not created by this tool, so Set up leaves it alone. A cluster-admin creates Secret %s/%s for it, or removes it.", maasDesc("Cluster", ns), ns, maasDBConfigSecret)
			out.blockers = append(out.blockers, part.Message)
		case st.clusterReady && st.secretFound:
			part.State, part.Message = "ready", fmt.Sprintf("CloudNativePG %s/%s runs (1 instance, development and test use; not in the MaaS docs: this tool's option); Secret %s set", ns, maasDBCluster, maasDBConfigSecret)
		case st.clusterReady:
			part.State, part.Message = "missing", "The database runs; its connection Secret is not written yet"
			out.plan = append(out.plan, fmt.Sprintf("Write Secret %s/%s with the database's address", ns, maasDBConfigSecret))
		case !st.clusterFound:
			part.State, part.Message = "missing", "Set up runs a one-instance CloudNativePG database (development and test use) in "+ns
			out.plan = append(out.plan, out.db.Choices[2].Plan...)
			if b := out.db.Choices[2].BlockedReason; b != "" {
				out.blockers = append(out.blockers, b)
			}
		default:
			part.State, part.Message = "waiting", "The database is starting: "+st.clusterPhase
		}
	}
	// Change database: only for a database this tool set up, that runs, and
	// while nobody else uses MaaS (their keys would be lost with it).
	if toolDBKind(st.kind) && part.State == "ready" {
		others, err := foreignMaaSObjects(c)
		switch {
		case err != nil:
			out.db.ChangeBlockedReason = "Cannot check whether others use MaaS: " + err.Error()
		case len(others) > 0:
			out.db.ChangeBlockedReason = "Others use this MaaS platform (" + strings.Join(others, ", ") + "); changing the database would lose their API keys."
		default:
			part.Actions = []types.MaaSAction{{ID: "change-database", Label: "Change database", Danger: true,
				Summary: "Switches maas-api to another database. Every API key is lost: the new database starts empty.",
				Changes: []string{"The Change database dialog lists what is created and deleted."}}}
		}
	}
	return done()
}

// cnpgStatusPart is the CloudNativePG operator's part, while the database
// is a CloudNativePG one.
func cnpgStatusPart(c *Client, rec maasRecord, cnpg operatorState, cr cnpgReadiness) types.MaaSPart {
	p := types.MaaSPart{ID: "cnpg", Title: "CloudNativePG operator", DocURL: maasDocsBase + "install/maas-setup.md#database-setup"}
	_, byTool := rec.Shared["cnpg"]
	switch {
	case cr.err != nil:
		p.State, p.Message = "unknown", cr.err.Error()
	case cr.served:
		p.ManagedByTool = byTool && cnpg.sub != nil && toolOwned(cnpg.sub.Metadata)
		p.NotManaged = !p.ManagedByTool
		p.State, p.Message = "ready", "Its API (postgresql.cnpg.io) is served"
		if cnpg.installed() {
			p.Message = fmt.Sprintf("Version %s runs in %s", cnpg.version, cnpg.csvNS)
		}
	case cr.pending:
		p.ManagedByTool, p.NotManaged = byTool, !byTool
		p.State, p.Message = "waiting", "OLM is installing it"
	case cr.planMsg != "":
		p.State, p.Message = "problem", "Not installed, and Set up cannot install it: "+cr.planMsg
	default:
		p.State = "missing"
		if cr.plan != nil {
			p.Message = fmt.Sprintf("Not installed. Set up installs version %s from certified-operators (channel %s) for this tool's database.", cr.plan.head, cr.plan.channel)
			p.Commands = cnpgInstallCommands(cr.plan.channel)
		}
	}
	return p
}

// --- making it --------------------------------------------------------------------------

// maasDBPlan is what Set up or Change database does about the database,
// checked before any write.
type maasDBPlan struct {
	state  maasDBState
	nsFrom bool // the namespace existed at the check
	// target is the kind to create or finish; "" for an adopted database
	// or "external" that is in place (nothing to do).
	target     string
	url        string
	cnpg       operatorState
	cnpgServed bool
	cnpgPlan   *operatorInstallPlan
	image      string
	imageFrom  string
	password   string
}

// planMaaSDatabase checks the database for Set up (switching false: what
// is missing, of the kind already chosen or of req) or Change database
// (true: req's kind instead of the current one). It returns the refusal's
// message and code instead.
func planMaaSDatabase(c *Client, rec maasRecord, ns string, nsFound bool, req *types.MaaSDatabaseRequest, switching bool) (*maasDBPlan, string, string) {
	p := &maasDBPlan{nsFrom: nsFound, state: maasDBState{ns: ns, poc: map[string]objectMeta{}}}
	var err error
	if p.state, err = readMaaSDBState(c, rec, ns); err != nil {
		return nil, fmt.Sprintf("Cannot read the database; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	cur := p.state.kind
	switch {
	case switching && !toolDBKind(cur):
		return nil, fmt.Sprintf("The database is %s, not one this tool set up, so this tool does not change it. Nothing was changed.", maasDBKindText(cur)), "not_managed"
	case switching && req.Kind == cur && cur != maasDBExternal:
		return nil, fmt.Sprintf("MaaS already uses %s. Nothing was changed.", maasDBKindText(cur)), "validation"
	case switching:
		p.target = req.Kind
	case cur == maasDBAdopted:
		return p, "", ""
	case cur != "":
		if req != nil && req.Kind != cur {
			return nil, fmt.Sprintf("The database is already chosen: %s. Change database on the Database part switches it (every API key is lost then). Nothing was changed.", maasDBKindText(cur)), "conflict"
		}
		if cur == maasDBExternal {
			// Set up never changes the URL in place; it would be ignored.
			if req != nil && req.ConnectionURL != "" {
				return nil, "Your PostgreSQL is set up already, and Set up does not change its connection URL. Change database on the Database part points MaaS at another URL (API keys are lost if it is a different database). Nothing was changed.", "conflict"
			}
			return p, "", ""
		}
		p.target = cur
	case req == nil:
		return nil, "Choose where MaaS keeps its API keys (Set up's database choice; reload the page if it does not show one). Nothing was changed.", "validation"
	default:
		p.target = req.Kind
	}
	switch p.target {
	case maasDBExternal:
		p.url = req.ConnectionURL
	case maasDBDevelopment:
		if foreign := p.state.foreignPOC(); foreign != "" {
			return nil, foreign + " exists and was not created by this tool (the docs' setup-database.sh uses the same names), so it is left alone. Its owner removes it, or choose another database. Nothing was changed.", "not_managed"
		}
		if p.password, err = maasPOCPassword(c, p.state); err != nil {
			return nil, fmt.Sprintf("Cannot prepare the development database's password; nothing was changed: %v.", err), errorCodeFromK8sErr(err)
		}
		p.image, p.imageFrom = maasPOCImage(c)
	case maasDBCNPG:
		if msg, code := p.planCNPG(c); msg != "" {
			return nil, msg, code
		}
	}
	return p, "", ""
}

func (p *maasDBPlan) planCNPG(c *Client) (string, string) {
	served, err := groupServed(c, cnpgGroup)
	if err != nil {
		return fmt.Sprintf("Cannot tell whether CloudNativePG is installed; nothing was changed: %v.", err), errorCodeFromK8sErr(err)
	}
	if served {
		if served, err = controllerDeployed(c, cnpgController); err != nil {
			return fmt.Sprintf("Cannot tell whether CloudNativePG's operator runs; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err)
		}
	}
	p.cnpgServed = served
	if !served {
		if p.cnpg, err = readOperator(c, cnpgOperator); err != nil {
			return fmt.Sprintf("Cannot read the installed operators; nothing was changed: %v.", err), errorCodeFromK8sErr(err)
		}
		if !p.cnpg.installed() && p.cnpg.pending() == nil {
			plan, msg, code := planOperatorInstall(c, cnpgOperator)
			if msg != "" {
				return msg + " Or choose another database.", code
			}
			p.cnpgPlan = plan
		}
	}
	if p.state.clusterFound && !toolOwned(p.state.cluster) {
		return fmt.Sprintf("%s exists and was not created by this tool, so it is left alone. Create Secret %s/%s for it yourself, or remove it, or choose another database. Nothing was changed.", maasDesc("Cluster", p.state.ns), p.state.ns, maasDBConfigSecret), "not_managed"
	}
	return "", ""
}

// connURL is the URL maas-db-config gets for the target (CloudNativePG's
// once its application Secret exists).
func (p *maasDBPlan) connURL(c *Client) (string, error) {
	switch p.target {
	case maasDBExternal:
		return p.url, nil
	case maasDBDevelopment:
		return maasPOCConnectionURL(p.password), nil
	}
	return maasConnectionURL(c, p.state.ns)
}

// dryRun checks every write of the database with the API server, where its
// namespace (and CloudNativePG's API) exist already.
// Its error may quote what was sent: callers redact it (p.redact).
func (p *maasDBPlan) dryRun(c *Client) (string, error) {
	ns := p.state.ns
	if p.cnpgPlan != nil {
		if _, err := createOperatorObjects(c, p.cnpgPlan, true); err != nil {
			return "CloudNativePG's install", err
		}
	}
	if !p.nsFrom {
		return "", nil
	}
	var secretURL string
	switch p.target {
	case maasDBDevelopment:
		for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
			meta, found := p.state.poc[k]
			if _, err := ensureMaaSDBObject(c, k, ns, maasPOCObject(k, ns, p.password, p.image), meta, found, true); err != nil {
				return maasDesc(k, ns), err
			}
		}
		secretURL = maasPOCConnectionURL(p.password)
	case maasDBCNPG:
		if !p.cnpgServed {
			return "", nil
		}
		if _, err := ensureMaaSDBObject(c, "Cluster", ns, maasDBClusterObject(ns), p.state.cluster, p.state.clusterFound, true); err != nil {
			return maasDesc("Cluster", ns), err
		}
		secretURL = "postgresql://dry-run@localhost/maas?sslmode=require"
	case maasDBExternal:
		secretURL = p.url
	default:
		return "", nil
	}
	if _, err := ensureMaaSDBObject(c, "Secret", ns, maasDBConfigObject(ns, secretURL), p.state.secret, p.state.secretFound, true); err != nil {
		return maasDesc("Secret", ns), err
	}
	return "", nil
}

// ensureMaaSDBObject creates or updates one of the database's objects
// (only this tool's).
func ensureMaaSDBObject(c *Client, kind, ns string, obj map[string]interface{}, cur objectMeta, found, dryRun bool) (objectMeta, error) {
	read := func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath(kind, ns)) }
	_, written, _, err := ensureToolObject(c, maasDesc(kind, ns), maasObjectPath(kind, ns), maasCreatePath(kind, ns), obj, cur, found, read, toolOwned, dryRun)
	return written, err
}

// ensureCNPG is the "cnpg" step: CloudNativePG installed for a cnpg
// target, else nothing.
func (m *maasRun) ensureCNPG(rec *maasRecord, p *maasDBPlan) (*types.OperationResponse, error) {
	c := m.c
	switch {
	case p.target != maasDBCNPG:
		m.step("cnpg", "success", "Not needed: the database is "+maasDBKindText(firstNonEmpty(p.target, p.state.kind))+".")
		return nil, nil
	case p.cnpgServed:
		m.step("cnpg", "success", "CloudNativePG is installed"+byWhom(rec.Shared["cnpg"] != "")+".")
		return nil, nil
	}
	ns, name := cnpgOperator.namespace, cnpgOperator.subName
	if p.cnpgPlan != nil {
		m.step("cnpg", "running", fmt.Sprintf("Installing CloudNativePG %s (channel %s) from certified-operators...", p.cnpgPlan.head, p.cnpgPlan.channel))
		rec.setShared("cnpg", fmt.Sprintf("Subscription %s/%s, channel %s", cnpgOperator.namespace, cnpgOperator.subName, p.cnpgPlan.channel))
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("cnpg", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
		made, err := createOperatorObjects(c, p.cnpgPlan, false)
		for _, d := range made {
			m.logs = append(m.logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.fail("cnpg", fmt.Sprintf("Failed to %v. Run it again to continue.", err), maasWriteCode(err), "cnpg install failed")
		}
	} else if pend := p.cnpg.pending(); pend != nil {
		ns, name = pend.Metadata.Namespace, pend.Metadata.Name
	}
	if p.cnpgPlan != nil || p.cnpg.pending() != nil {
		if r, msg := m.waitOperator("cnpg", ns, name, cnpgOperator.title); r != nil {
			return r, msg
		}
	}
	served, err := waitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) { return groupServed(cc, cnpgGroup) })
	if err != nil || !served {
		return m.fail("cnpg", fmt.Sprintf("CloudNativePG's API (postgresql.cnpg.io) is not served after %s%s. Run it again to continue.", clockText(MaaSOperatorTimeout), errSuffix(err)), firstNonEmpty(errorCodeFromK8sErr(err), "not_ready"), "cnpg api missing")
	}
	p.cnpgServed = true
	m.step("cnpg", "success", "CloudNativePG runs.")
	return nil, nil
}

// makeDatabase is the "database" step: the target's objects, a wait until
// it runs, then maas-db-config with its address (maas-api restarted to
// read it when it runs). switching keeps the record's kind until the
// Secret names the new database.
func (m *maasRun) makeDatabase(rec *maasRecord, p *maasDBPlan, maasOn, switching bool) (*types.OperationResponse, error) {
	c := m.c
	ns := p.state.ns
	switch {
	case p.state.kind == maasDBAdopted:
		m.step("database", "success", fmt.Sprintf("Secret %s/%s already exists and was not written by this tool: maas-api uses the database it names, and this tool does not change it.", ns, maasDBConfigSecret))
		return nil, nil
	case p.target == "":
		m.step("database", "success", fmt.Sprintf("Secret %s/%s, written by this tool for your PostgreSQL, is in place.", ns, maasDBConfigSecret))
		return nil, nil
	}
	// A database this tool makes is recorded before its objects, so Remove
	// finds them; the user's only after its Secret is written, so a failed
	// write is not taken for a lost Secret (writeDBConfig).
	if !switching && p.target != maasDBExternal && (rec.Database != p.target || rec.DBNamespace != ns) {
		rec.Database, rec.DBNamespace = p.target, ns
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
	}
	// Read again: the namespace or CloudNativePG's API may be new.
	st, err := readMaaSDBState(c, *rec, ns)
	if err != nil {
		return m.fail("database", fmt.Sprintf("Cannot read the database: %v.", err), errorCodeFromK8sErr(err), "db read failed")
	}
	switch p.target {
	case maasDBDevelopment:
		if foreign := st.foreignPOC(); foreign != "" {
			return m.fail("database", foreign+" exists and was not created by this tool, so it is left alone.", "not_managed", "db foreign")
		}
		m.step("database", "running", fmt.Sprintf("Creating the MaaS docs' development database in %s (scripts/setup-database.sh; image %s, %s)...", ns, p.image, p.imageFrom))
		for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
			meta, found := st.poc[k]
			if _, err := ensureMaaSDBObject(c, k, ns, maasPOCObject(k, ns, p.password, p.image), meta, found, false); err != nil {
				return m.fail("database", p.redact(fmt.Sprintf("Failed to apply %s: %v. Run it again to continue.", maasDesc(k, ns), err)), maasWriteCode(err), "db apply failed")
			}
			m.logs = append(m.logs, "OK: "+maasDesc(k, ns))
		}
		m.step("database", "running", "Waiting for the development database to be available (up to "+clockText(MaaSDevDatabaseTimeout)+", as the script waits)...")
		ready, err := waitForGone(c, MaaSDevDatabaseTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			return deploymentAvailable(cc, ns, maasPOCName) == "True", nil
		})
		if err != nil {
			return m.fail("database", fmt.Sprintf("Stopped waiting for the database: %v. It was created; run it again to continue.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "db wait failed")
		}
		if !ready {
			return m.fail("database", fmt.Sprintf("The development database is not available after %s (the script fails here too). Run it again to continue; the MaaS tab shows its state.", clockText(MaaSDevDatabaseTimeout)), "not_ready", "db not ready")
		}
	case maasDBCNPG:
		if st.clusterFound && !toolOwned(st.cluster) {
			return m.fail("database", fmt.Sprintf("%s exists and was not created by this tool, so it is left alone. Create Secret %s/%s for it yourself, or remove it.", maasDesc("Cluster", ns), ns, maasDBConfigSecret), "not_managed", "db foreign")
		}
		m.step("database", "running", fmt.Sprintf("Creating a one-instance CloudNativePG database %s/%s (for development and test)...", ns, maasDBCluster))
		if _, err := ensureMaaSDBObject(c, "Cluster", ns, maasDBClusterObject(ns), st.cluster, st.clusterFound, false); err != nil {
			return m.fail("database", fmt.Sprintf("Failed to apply %s: %v.", maasDesc("Cluster", ns), err), maasWriteCode(err), "db apply failed")
		}
		m.step("database", "running", "Waiting for the database to start (usually 1 to 2 minutes)...")
		ready, err := waitForGone(c, MaaSDatabaseTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			_, _, ok, _, err := readMaaSDBCluster(cc, ns)
			return ok, err
		})
		if err != nil {
			return m.fail("database", fmt.Sprintf("Stopped waiting for the database: %v. It was created; run it again to continue.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "db wait failed")
		}
		if !ready {
			return m.fail("database", fmt.Sprintf("The database is not running after %s, and maas-api needs it before MaaS is turned on. Run it again to continue; the MaaS tab shows its state.", clockText(MaaSDatabaseTimeout)), "not_ready", "db not ready")
		}
	}
	return m.writeDBConfig(rec, p, st, maasOn, switching)
}

// writeDBConfig writes maas-db-config for the target, and restarts a
// running maas-api so it reads it (maas-setup.md: "Secret changes continue
// to require a maas-api pod restart"). No message carries the URL.
func (m *maasRun) writeDBConfig(rec *maasRecord, p *maasDBPlan, st maasDBState, maasOn, switching bool) (*types.OperationResponse, error) {
	c := m.c
	ns := st.ns
	connURL, err := p.connURL(c)
	if err != nil {
		return m.fail("database", err.Error()+" Run it again to retry.", errorCodeFromK8sErr(err), "db secret read failed")
	}
	written, err := ensureMaaSDBObject(c, "Secret", ns, maasDBConfigObject(ns, connURL), st.secret, st.secretFound, false)
	if err != nil {
		return m.fail("database", p.redact(fmt.Sprintf("Failed to write Secret %s/%s: %v.", ns, maasDBConfigSecret, err), connURL), maasWriteCode(err), "secret apply failed")
	}
	if switching || rec.Database != p.target || rec.DBNamespace != ns {
		rec.Database, rec.DBNamespace = p.target, ns
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("database", err.Error()+" The Secret names the new database already.", errorCodeFromK8sErr(err), "record write failed")
		}
	}
	// The API server keeps the resourceVersion when an apply changes nothing.
	if changed := !st.secretFound || written.ResourceVersion != st.secret.ResourceVersion; changed && maasOn {
		restarted, err := restartMaaSAPI(c, ns)
		if err != nil {
			return m.fail("database", fmt.Sprintf("Secret %s/%s was written, but maas-api could not be restarted to read it: %v. Restart it (oc rollout restart deployment/maas-api -n %s) or run it again.", ns, maasDBConfigSecret, err, ns), errorCodeFromK8sErr(err), "maas-api restart failed")
		}
		if restarted {
			m.step("database", "running", "maas-api restarts to read the new connection Secret.")
		}
	}
	m.step("database", "success", fmt.Sprintf("Secret %s/%s names %s.", ns, maasDBConfigSecret, maasDBKindText(p.target)))
	return nil, nil
}

// deleteDBObjects deletes the objects of a database of dbKind this tool
// created in ns, besides maas-db-config (UID preconditions; "external" has
// none: the database is the user's), and returns what it deleted.
func (m *maasRun) deleteDBObjects(step, ns, dbKind string) ([]string, *types.OperationResponse) {
	c := m.c
	var deleted []string
	kinds := []string{}
	switch dbKind {
	case maasDBDevelopment:
		kinds = []string{"PostgresDeployment", "PostgresService", "PostgresSecret"}
	case maasDBCNPG:
		kinds = []string{"Cluster"}
	}
	for _, k := range kinds {
		path := maasObjectPath(k, ns)
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			res, _ := m.fail(step, fmt.Sprintf("Cannot read %s: %v. Run it again to retry.", maasDesc(k, ns), err), errorCodeFromK8sErr(err), "read failed")
			return deleted, res
		}
		if !found {
			continue
		}
		if !toolOwned(meta) {
			m.logs = append(m.logs, "Kept "+maasDesc(k, ns)+": not created by this tool")
			continue
		}
		m.step(step, "running", "Deleting "+maasDesc(k, ns)+"...")
		if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
			res, _ := m.fail(step, fmt.Sprintf("Failed to delete %s: %v. Run it again to retry.", maasDesc(k, ns), err), maasWriteCode(err), "delete failed")
			return deleted, res
		}
		if gone, _ := waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll); !gone {
			m.logs = append(m.logs, maasDesc(k, ns)+" is still being deleted")
		}
		deleted = append(deleted, maasDesc(k, ns))
	}
	return deleted, nil
}

// --- Change database ------------------------------------------------------------------

// maasDBNotReady says why the database this tool set up does not run
// (what the status reports as not ready), or "".
func maasDBNotReady(c *Client, st maasDBState) string {
	switch st.kind {
	case maasDBDevelopment:
		for _, k := range []string{"PostgresSecret", "PostgresDeployment", "PostgresService"} {
			if _, ok := st.poc[k]; !ok {
				return maasDesc(k, st.ns) + " is missing."
			}
		}
		if !st.secretFound {
			return maasDesc("Secret", st.ns) + " is missing."
		}
		if deploymentAvailable(c, st.ns, maasPOCName) != "True" {
			return "The development database is not available."
		}
	case maasDBCNPG:
		if !st.clusterReady || !st.secretFound {
			return "The CloudNativePG database is not ready, or its connection Secret is not written."
		}
	}
	return ""
}

// changeDatabase switches maas-api to another database this tool makes (or
// to the user's): the new one first, then maas-db-config and a restart of
// maas-api, then the old one's objects. Every API key is lost (the request
// confirms it). It refuses for a database this tool did not set up, and
// while others use MaaS. Steps: "check", "cnpg", "database", "cleanup".
func (m *maasRun) changeDatabase(req *types.MaaSDatabaseRequest) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the database and who uses MaaS...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	dsc, err := readMaaSDSC(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "dsc read failed")
	}
	ns := firstNonEmpty(rec.DBNamespace, maasInfraNamespace(c))
	_, nsFound, err := readObjectMeta(c, "/api/v1/namespaces/"+ns)
	if err != nil || !nsFound {
		return m.fail("check", fmt.Sprintf("Cannot read namespace %s, where the database is; nothing was changed%s.", ns, errSuffix(err)), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites"), "namespace read failed")
	}
	if others, err := foreignMaaSObjects(c); err != nil {
		return m.fail("check", fmt.Sprintf("Cannot check whether others use MaaS, so the database was not changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "maas list failed")
	} else if len(others) > 0 {
		return m.fail("check", fmt.Sprintf("Others use this MaaS platform: %s. Changing the database would lose their API keys too, so it was not changed.", strings.Join(others, ", ")), "in_use", "maas in use")
	}
	p, msg, code := planMaaSDatabase(c, rec, ns, true, req, true)
	if msg != "" {
		return m.fail("check", msg, code, "database check failed")
	}
	old := p.state.kind
	if why := maasDBNotReady(c, p.state); why != "" {
		return m.fail("check", why+" Set up finishes it; Change database runs only from a database that runs. Nothing was changed.", "not_ready", "database not ready")
	}
	if what, err := p.dryRun(c); err != nil {
		return m.fail("check", p.redact(fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, templateHint(err))), maasWriteCode(err), "dry run failed")
	}
	m.step("check", "success", fmt.Sprintf("MaaS can switch from %s to %s.", maasDBKindText(old), maasDBKindText(p.target)))
	if r, err := m.ensureCNPG(&rec, p); r != nil || err != nil {
		return r, err
	}
	if r, err := m.makeDatabase(&rec, p, dsc.maas == "Managed", true); r != nil || err != nil {
		return r, err
	}
	var deleted []string
	if old != p.target {
		var res *types.OperationResponse
		if deleted, res = m.deleteDBObjects("cleanup", ns, old); res != nil {
			return res, nil
		}
	}
	switch {
	case len(deleted) > 0:
		m.step("cleanup", "success", "Deleted the database before: "+strings.Join(deleted, ", ")+". The CloudNativePG operator stays if it is installed: it is shared.")
	case old == maasDBExternal:
		m.step("cleanup", "success", "Your PostgreSQL before is not touched: this tool never changes or deletes it.")
	default:
		m.step("cleanup", "success", "Nothing of the database before is left to delete.")
	}
	return m.done(fmt.Sprintf("MaaS now uses %s. API keys made before are gone: create them again.", maasDBKindText(p.target)))
}
