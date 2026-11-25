package modelregistry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

var (
	// readyTimeout bounds the wait for the registry to report Available
	// (the operator starts its database, then the server).
	readyTimeout = 10 * time.Minute
	// deleteTimeout bounds the wait for the registry to go (its finalizer
	// runs the operator's clean-up).
	deleteTimeout = 5 * time.Minute
	poll          = envkit.PollInterval
)

func jsonBody(in any) ([]byte, error) { return json.Marshal(in) }

// ValidateRequest returns why req cannot run, or "".
func ValidateRequest(req types.ModelRegistryRequest) string {
	switch req.Action {
	case "setup":
		return ""
	case "teardown":
		if req.Samples {
			return "samples is a setup option"
		}
		return ""
	}
	return "action must be setup or teardown"
}

// Run sets the registry up ("check", "create", "ready", then "samples" when
// asked) or deletes it ("check", "delete", "gone"), as one operation
// streaming its steps. Every change is made with c, the signed-in user's
// own client.
func Run(c *kube.Client, req types.ModelRegistryRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return RunAnnotated(c, req, nil, onStep)
}

// RunAnnotated is Run, with annotations (setup only) added to the registry
// it creates: an environment recipe marks the registry its run created
// (a registry that exists already is not changed).
func RunAnnotated(c *kube.Client, req types.ModelRegistryRequest, annotations map[string]string, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &serving.Run{Client: c, Emit: onStep, Activity: req.Action + "-model-registry", Target: "model registry " + Name}
	if problem := ValidateRequest(req); problem != "" {
		return m.Fail("check", problem, "validation", "invalid request")
	}
	if req.Action == "teardown" {
		return teardown(m)
	}
	return setup(m, req.Samples, annotations)
}

// readPlacement reads where registries go, with c (the user's client).
func readPlacement(c *kube.Client) (placement, error) {
	r, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return placement{}, err
	}
	return placementOf(r), nil
}

func setup(m *serving.Run, samples bool, annotations map[string]string) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("check", "running", "Checking that the cluster can run a model registry")
	p, err := readPlacement(c)
	switch {
	case err != nil:
		return m.Fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "dsc read failed")
	case p.block != nil:
		return m.Fail("check", p.block.Reason+" Set up its prerequisites first. Nothing was changed.", "prerequisites", "component not managed")
	case p.namespace == "":
		return m.Fail("check", "RHOAI does not report where model registries go yet (the DataScienceCluster's component status names no namespace). Wait until its operator is ready, then set up again. Nothing was changed.", "prerequisites", "no namespace")
	}
	ns := p.namespace
	if v := rhoai.ServedVersions(c, Group, nil); !slices.Contains(v.Versions, Version) {
		msg := fmt.Sprintf("This cluster does not serve the ModelRegistry API at %s, which the dashboard lists registries with, so a registry made here would not show there. Nothing was changed.", APIVersion)
		if !v.Discovered {
			msg = fmt.Sprintf("This cluster does not serve the ModelRegistry API (%s): RHOAI's operator has not installed it yet. Wait until component %s is ready, then set up again. Nothing was changed.", Group, p.component)
		}
		return m.Fail("check", msg, "prerequisites", "api not served")
	}
	rec, err := ReadRecord(c)
	if err != nil {
		return m.Fail("check", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record read failed")
	}
	o, found, err := readCR(c, ns)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read ModelRegistry %s/%s; nothing was changed: %v.%s", ns, Name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "cr read failed")
	}
	switch {
	case found && !rec.Owns(o.Metadata):
		return m.Fail("check", fmt.Sprintf("A model registry named %s exists in %s that this tool did not create. This tool does not change or delete it. Nothing was changed.", Name, ns), "not_managed", "foreign registry")
	case found && o.Metadata.Terminating():
		return m.Fail("check", fmt.Sprintf("Model registry %s/%s is being deleted. Set up again once it is gone. Nothing was changed.", ns, Name), "in_progress", "terminating")
	case !found:
		// The dashboard's own order: a server-side dry run, then the
		// create. Strict field validation makes the API server refuse a
		// field the CRD does not have instead of dropping it, so a cluster
		// without the default database is refused before anything changes.
		if err := create(c, ns, annotations, true); err != nil {
			if why := unknownFieldsError(err); why != nil {
				return m.Fail("check", why.Error()+". Nothing was changed.", "prerequisites", "no default database")
			}
			return m.Fail("check", fmt.Sprintf("The API server rejects ModelRegistry %s/%s: %v. Nothing was changed.%s", ns, Name, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	m.Step("check", "success", fmt.Sprintf("Component %s is Managed; registries go in %s.", p.component, ns))

	// 1. The registry.
	uid := o.Metadata.UID
	if found {
		m.Step("create", "success", fmt.Sprintf("Model registry %s/%s exists (created by this tool).", ns, Name))
	} else {
		m.Step("create", "running", fmt.Sprintf("Creating model registry %s/%s with the default database", ns, Name))
		if err := create(c, ns, annotations, false); err != nil {
			if kube.IsK8sConflict(err) {
				return m.Fail("create", fmt.Sprintf("A model registry named %s was created in %s by someone else meanwhile. Nothing was changed.", Name, ns), "conflict", "create conflict")
			}
			return m.Fail("create", fmt.Sprintf("Failed to create model registry %s/%s: %v.%s", ns, Name, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "create failed")
		}
		made, _, err := readCR(c, ns)
		if err == nil && made.Metadata.UID == "" {
			err = fmt.Errorf("it has no UID")
		}
		if err == nil {
			uid = made.Metadata.UID
			rec[ns+"/"+Name] = Recorded{UID: uid, At: kube.NowRFC3339(), By: kube.GetUser(c)}
			err = writeRecord(c, rec)
		}
		if err != nil {
			return m.Fail("create", fmt.Sprintf("Model registry %s/%s was created, but this tool could not record it as its own (%v), so it shows as not created by this tool and Clean up does not delete it. Delete it in the dashboard (Settings, Model registry settings).", ns, Name, err), kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		m.Logs = append(m.Logs, "OK: ModelRegistry "+ns+"/"+Name+" created")
		m.Step("create", "success", fmt.Sprintf("Model registry %s/%s created; its operator starts a PostgreSQL database and the registry server.", ns, Name))
	}

	// 2. Ready.
	address, problem, res, err := waitReady(m, ns, uid)
	if res != nil || err != nil {
		return res, err
	}

	// 3. The sample model.
	if samples {
		switch {
		case problem != "":
			return m.Fail("samples", fmt.Sprintf("The registry is ready, but the sample model was not added: %s. Nothing was sent: your token goes only to the registry on the cluster's own gateway.", problem), "untrusted_address", "untrusted address")
		case address == "":
			m.Step("samples", "success", "Skipped: the registry has no address through the cluster's gateway, which sample data is added through.")
		default:
			m.Step("samples", "running", fmt.Sprintf("Registering sample model %s", SampleName()))
			if err := registerSample(c, address, func(s string) { m.Logs = append(m.Logs, s) }); err != nil {
				code := mlflow.SampleErrorCode(err)
				hint := ""
				if mlflow.IsDenied(err) {
					hint = " The registry checks your own OpenShift permissions (get on its Service), or your sign-in expired: log out and log in again."
				}
				return m.Fail("samples", fmt.Sprintf("The registry is ready, but the sample model could not be registered: %v.%s Set up again to retry.", err, hint), code, "samples failed")
			}
			m.Step("samples", "success", fmt.Sprintf("Sample model %s (version %s) is registered.", SampleName(), sampleVersion))
		}
	}
	return m.Done(fmt.Sprintf("Model registry %s is ready in %s.", Name, ns))
}

// create posts the registry (a server-side dry run with dryRun), with
// strict field validation.
func create(c *kube.Client, ns string, annotations map[string]string, dryRun bool) error {
	data, _ := json.Marshal(BodyWith(ns, annotations))
	query := url.Values{"fieldValidation": {"Strict"}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	_, _, err := c.Do(http.MethodPost, envkit.WithToolFieldManager(collectionPath(ns)), "application/json", data, query)
	return err
}

// waitReady waits for Available=True on the registry, streaming its
// message as it changes, then reads the Service's gateway address.
func waitReady(m *serving.Run, ns, uid string) (string, string, *types.OperationResponse, error) {
	c := m.Client
	m.Step("ready", "running", "Waiting for the registry to be available (usually 1 to 3 minutes)")
	last := ""
	done, err := kube.WaitForGone(c, readyTimeout, poll, func(cc *kube.Client) (bool, error) {
		o, found, err := readCR(cc, ns)
		if err != nil {
			return false, err
		}
		if !found {
			return false, fmt.Errorf("model registry %s/%s is gone", ns, Name)
		}
		s, msg := o.available()
		if s == "True" {
			return true, nil
		}
		if msg = kube.TruncateText(msg, 300); msg != "" && msg != last {
			last = msg
			m.Step("ready", "running", "Waiting: "+msg)
		}
		return false, nil
	})
	if err != nil {
		r, e := m.Fail("ready", fmt.Sprintf("Stopped waiting: %v. The registry stays; the tab shows its state.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
		return "", "", r, e
	}
	if !done {
		r, e := m.Fail("ready", fmt.Sprintf("Not available after %s (%s). The registry stays; the tab shows its state.", serving.ClockText(readyTimeout), kube.FirstNonEmpty(last, "no message")), "not_ready", "not ready")
		return "", "", r, e
	}
	svc, found, err := readService(c, ns)
	if err != nil {
		r, e := m.Fail("ready", fmt.Sprintf("The registry is available, but its Service cannot be read: %v.", err), kube.ErrorCodeFromK8sErr(err), "service read failed")
		return "", "", r, e
	}
	address, problem := "", ""
	if found {
		address, problem = trustedAddress(svc, uid, dashdev.DashboardURL(c))
	}
	where := "it has no address through the cluster's gateway"
	switch {
	case address != "":
		where = "its REST address is " + address
	case problem != "":
		where = "its address is not used: " + problem
	}
	m.Step("ready", "success", fmt.Sprintf("The registry is available; %s.", where))
	return address, problem, nil, nil
}

// teardown deletes the registry this tool created: only when its UID is
// the recorded one and it carries the tool's label. Its default database
// (Secret, PVC, Deployment and Service, each owned by the registry) goes
// with it, and with that every registered model.
func teardown(m *serving.Run) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("check", "running", "Checking the model registry")
	rec, err := ReadRecord(c)
	if err != nil {
		return m.Fail("check", err.Error()+" Nothing was deleted.", kube.ErrorCodeFromK8sErr(err), "record read failed")
	}
	// The recorded registry, wherever the component put it.
	var ns string
	for key := range rec {
		if n, name, ok := splitKey(key); ok && name == Name {
			ns = n
		}
	}
	if ns == "" {
		m.Step("check", "success", "This tool has no model registry recorded; nothing to delete.")
		return m.Done("No model registry of this tool to delete.")
	}
	o, found, err := readCR(c, ns)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read ModelRegistry %s/%s; nothing was deleted: %v.%s", ns, Name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "cr read failed")
	}
	if !found {
		delete(rec, ns+"/"+Name)
		if err := writeRecord(c, rec); err != nil {
			m.Logs = append(m.Logs, "Could not update this tool's record: "+err.Error())
		}
		m.Step("check", "success", fmt.Sprintf("Model registry %s/%s is gone already.", ns, Name))
		return m.Done("The model registry is gone already.")
	}
	if !rec.Owns(o.Metadata) {
		return m.Fail("check", fmt.Sprintf("Model registry %s/%s is not the one this tool created (its UID or label differs), so it was not deleted.", ns, Name), "not_managed", "not managed")
	}
	m.Step("check", "success", fmt.Sprintf("Model registry %s/%s is the one this tool created.", ns, Name))

	path := crPath(ns)
	if !o.Metadata.Terminating() {
		m.Step("delete", "running", fmt.Sprintf("Deleting model registry %s/%s with its database", ns, Name))
		if _, err := kube.DeleteWithUID(c, path, o.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return m.Fail("delete", fmt.Sprintf("Failed to delete model registry %s/%s: %v.%s", ns, Name, err, kube.TemplateHint(err)), code, "delete failed")
		}
		m.Logs = append(m.Logs, "OK: ModelRegistry "+ns+"/"+Name+" deleted")
	}
	m.Step("delete", "success", fmt.Sprintf("Model registry %s/%s is being deleted; its operator and the garbage collector remove its database.", ns, Name))

	m.Step("gone", "running", "Waiting for the registry to be gone")
	gone, err := kube.WaitForDeletion(c, path, deleteTimeout, poll)
	if err != nil {
		return m.Fail("gone", fmt.Sprintf("Deletion was requested, but whether the registry is gone cannot be checked: %v.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "in_progress"), "gone check failed")
	}
	if !gone {
		return m.Fail("gone", fmt.Sprintf("Deletion was requested, but the registry still exists after %s (its operator's finalizer has not finished). Clean up again to check.", serving.ClockText(deleteTimeout)), "in_progress", "still terminating")
	}
	delete(rec, ns+"/"+Name)
	if err := writeRecord(c, rec); err != nil {
		m.Logs = append(m.Logs, "Could not update this tool's record: "+err.Error())
	}
	m.Step("gone", "success", "The registry and its database are gone.")
	return m.Done(fmt.Sprintf("Model registry %s removed, with its registered models and its database.", Name))
}

// splitKey splits a record key "<namespace>/<name>".
func splitKey(key string) (ns, name string, ok bool) {
	for i := range len(key) {
		if key[i] == '/' {
			return key[:i], key[i+1:], i > 0 && i < len(key)-1
		}
	}
	return "", "", false
}

// Users lists the model registries in ns (where RHOAI keeps them), those
// being deleted included: whether the component is still in use, for
// turning it back off (Clean up, with the user's token). A list that fails is
// returned, so the caller keeps the component on (fail closed); an API that
// is not served has no registries.
func Users(c *kube.Client, ns string) ([]string, error) {
	body, found, err := c.GetOptional(collectionPath(ns))
	if err != nil {
		return nil, fmt.Errorf("list the model registries in %s: %w", ns, err)
	}
	if !found {
		return nil, nil
	}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse the model registries in %s: %w", ns, err)
	}
	// One being deleted still counts until it is gone: its finalizer needs
	// the component's operator to run.
	var out []string
	for _, it := range list.Items {
		name := fmt.Sprintf("model registry %s/%s", ns, it.Metadata.Name)
		if it.Metadata.Terminating() {
			name += " (being deleted)"
		}
		out = append(out, name)
	}
	return out, nil
}
