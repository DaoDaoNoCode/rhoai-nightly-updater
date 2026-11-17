package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"log/slog"
	"mime/multipart"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The sample pipeline: a two-step pipeline (write an artifact, read it
// back) in a project whose pipeline server this tool created
// (nightly-dspa), run through the KFP v2beta1 API of
// red-hat-data-services/data-science-pipelines rhoai-3.6 (93953a7a).
//
// The API is reached on the DSPA's own address, which DSPO reports in
// status.components.apiServer: url is the Service
// (https://ds-pipeline-<dspa>.<ns>.svc.cluster.local:8443, the
// kube-rbac-proxy) and externalUrl the Route to the same port
// (data-science-pipelines-operator rhoai-3.6 controllers/util/util.go:
// 135-158, dspipeline_controller.go:908-960, apiserver/route/
// route.yaml.tmpl). In the pod the Service is used (its NetworkPolicy admits
// port 8443 from any pod: common/default/policy.yaml.tmpl:15-22); in
// DEV_MODE, which runs outside the cluster, the Route. Only an https
// address is used: without its route the server exposes only the
// unauthenticated port 8888 (apiserver/default/service.yaml.tmpl:13-25).
//
// What the tool creates, all named pipelineSampleName and described with
// pipelineSampleDescription (KFP experiments have no labels or tags in
// every release; the description is the ownership proof):
//   - a KFP experiment, so the sample runs never land in the project's
//     Default experiment (which cannot be deleted: resource_manager.go:
//     221-228);
//   - a pipeline with one version, uploaded from pipelineSampleSpec
//     (POST /apis/v2beta1/pipelines/upload, pipeline_upload_server.go:
//     96-230), so it also shows on the dashboard's Pipelines page;
//   - one run per "Run sample pipeline".
//
// Remove deletes, in order: the runs of the sample pipeline in the sample
// experiment (DeleteRun deletes the Argo Workflow, then the row:
// resource_manager.go:933-972), the pipeline's versions, the pipeline, and
// the experiment last, which deletes no runs itself (storage/
// experiment_store.go:271-302). It keeps the experiment when it holds runs
// the tool did not start. KFP keeps the artifacts in S3 and the MLMD
// records; the storage's own Tear down removes the S3 data.
const (
	pipelineSampleName        = "rhoai-nightly-sample"
	pipelineSampleDescription = "Sample created by rhoai-nightly-updater: a two-step pipeline that writes an artifact and reads it back."
	// pipelineSampleDefaultImage is UBI 9 minimal, pinned by digest (the
	// multi-arch manifest list, amd64/arm64/s390x/ppc64le, read from
	// registry.access.redhat.com on 2026-11-17). Both steps only need sh,
	// mkdir, dirname, echo and cat. On a disconnected cluster mirror it or
	// set SAMPLE_PIPELINE_IMAGE.
	pipelineSampleDefaultImage = "registry.access.redhat.com/ubi9/ubi-minimal@sha256:baafce2d34783a28b4a472779f304e97e00f9d35c99553bc53b068b14344d9a9"
	pipelineSampleCacheTTL     = 5 * time.Second
	kfpAPI                     = "/apis/v2beta1"
)

var pipelineSampleCache = newLRU[string, types.PipelineSampleStatus](64)

// pipelineSampleImage is the image both steps run: SAMPLE_PIPELINE_IMAGE,
// else the pinned default. A changed image applies to a pipeline uploaded
// after it (Remove, then Run).
func pipelineSampleImage() string {
	if img := strings.TrimSpace(os.Getenv("SAMPLE_PIPELINE_IMAGE")); img != "" {
		return img
	}
	return pipelineSampleDefaultImage
}

// pipelineSampleSpec is the pipeline in KFP's IR (PipelineSpec, schema
// 2.1.0) with two shell-only container components. The launcher replaces
// the artifact path placeholders; "sh -c script path" passes the path as
// $0. Caching is off so every run executes.
//
// It is built as data and marshalled, never templated as text: a templated
// description with ": " in it was invalid YAML, and KFP's upload handler
// hung on it until the route answered 504 (seen live on 3.6).
func pipelineSampleSpec(image string) string {
	artifact := map[string]interface{}{
		"artifactType": map[string]interface{}{"schemaTitle": "system.Artifact", "schemaVersion": "0.0.1"},
	}
	container := func(script, path string) map[string]interface{} {
		return map[string]interface{}{"container": map[string]interface{}{
			"image": image, "command": []string{"sh", "-c"}, "args": []string{script, path},
		}}
	}
	spec := map[string]interface{}{
		"components": map[string]interface{}{
			"comp-write-artifact": map[string]interface{}{
				"executorLabel":     "exec-write-artifact",
				"outputDefinitions": map[string]interface{}{"artifacts": map[string]interface{}{"greeting": artifact}},
			},
			"comp-read-artifact": map[string]interface{}{
				"executorLabel":    "exec-read-artifact",
				"inputDefinitions": map[string]interface{}{"artifacts": map[string]interface{}{"greeting": artifact}},
			},
		},
		"deploymentSpec": map[string]interface{}{"executors": map[string]interface{}{
			"exec-write-artifact": container(`mkdir -p "$(dirname "$0")" && echo "Hello from the rhoai-nightly-updater sample pipeline" > "$0" && echo "Wrote $0"`,
				"{{$.outputs.artifacts['greeting'].path}}"),
			"exec-read-artifact": container(`echo "Read $0:" && cat "$0"`, "{{$.inputs.artifacts['greeting'].path}}"),
		}},
		"pipelineInfo": map[string]interface{}{"name": pipelineSampleName, "description": pipelineSampleDescription},
		"root": map[string]interface{}{"dag": map[string]interface{}{"tasks": map[string]interface{}{
			"write-artifact": map[string]interface{}{
				"cachingOptions": map[string]interface{}{},
				"componentRef":   map[string]interface{}{"name": "comp-write-artifact"},
				"taskInfo":       map[string]interface{}{"name": "write-artifact"},
			},
			"read-artifact": map[string]interface{}{
				"cachingOptions": map[string]interface{}{},
				"componentRef":   map[string]interface{}{"name": "comp-read-artifact"},
				"dependentTasks": []string{"write-artifact"},
				"inputs": map[string]interface{}{"artifacts": map[string]interface{}{"greeting": map[string]interface{}{
					"taskOutputArtifact": map[string]interface{}{"outputArtifactKey": "greeting", "producerTask": "write-artifact"},
				}}},
				"taskInfo": map[string]interface{}{"name": "read-artifact"},
			},
		}}},
		"schemaVersion": "2.1.0",
		"sdkVersion":    "kfp-2.13.0",
	}
	out, err := yaml.Marshal(spec)
	if err != nil {
		// Marshalling plain maps of strings cannot fail; a panic here is a
		// programming error caught by the tests.
		panic(fmt.Sprintf("marshal the sample pipeline: %v", err))
	}
	return string(out)
}

// runsInCluster reports whether the app runs in a pod (a ServiceAccount
// token is mounted), so Service addresses resolve. Tests replace it.
var runsInCluster = func() bool {
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return err == nil
}

// kfpTarget is the KFP API of the project's tool-created pipeline server.
type kfpTarget struct {
	api       *sampleAPI
	project   string
	dashboard string // RHOAI dashboard address, "" when unknown
}

func (k *kfpTarget) runLink(id string) string {
	if k.dashboard == "" {
		return ""
	}
	return k.dashboard + "/develop-train/pipelines/runs/" + k.project + "/runs/" + id
}

// pipelineSampleTarget returns the KFP API of project's nightly-dspa, or
// why the sample is unavailable.
func pipelineSampleTarget(c *Client, project string) (*kfpTarget, string, error) {
	path, _ := toolDSPAPath(project, dspaName)
	body, _, err := c.get(path)
	switch {
	case IsK8sError(err, 404):
		return nil, "This project has no pipeline server created by this tool. Set one up first.", nil
	case err != nil:
		return nil, "", fmt.Errorf("read pipeline server %s/%s: %w", project, dspaName, err)
	}
	var dspa struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			Components struct {
				APIServer struct {
					URL         string `json:"url"`
					ExternalURL string `json:"externalUrl"`
				} `json:"apiServer"`
			} `json:"components"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &dspa); err != nil {
		return nil, "", fmt.Errorf("parse pipeline server %s/%s: %w", project, dspaName, err)
	}
	if !dspa.Metadata.hasToolLabel() {
		return nil, "The pipeline server '" + dspaName + "' was not created by this tool.", nil
	}
	if dspa.Metadata.terminating() {
		return nil, "The pipeline server is being deleted.", nil
	}
	ready := false
	for _, cond := range dspa.Status.Conditions {
		if cond.Type == "Ready" {
			ready = cond.Status == "True"
		}
	}
	if !ready {
		return nil, "The pipeline server is not ready yet.", nil
	}
	address := dspa.Status.Components.APIServer.URL
	if !runsInCluster() {
		address = dspa.Status.Components.APIServer.ExternalURL
	}
	base := httpsBaseURL(address)
	if base == "" {
		return nil, "The pipeline server reports no https API address (status.components.apiServer); its authenticated endpoint may be turned off.", nil
	}
	return &kfpTarget{
		api:     &sampleAPI{ctx: c.ctx, base: base, token: c.token, server: "the pipeline server"},
		project: project, dashboard: dashboardBaseURL(c),
	}, "", nil
}

// kfpNotFound: KFP answers a missing object with HTTP 404 (grpc NotFound).
func kfpNotFound(err error) bool {
	e, ok := asSampleAPIError(err)
	return ok && e.Status == 404
}

func displayNameFilter(key string) string {
	f, _ := json.Marshal(map[string]interface{}{"predicates": []map[string]interface{}{{"key": key, "operation": "EQUALS", "string_value": pipelineSampleName}}})
	return string(f)
}

type kfpExperiment struct {
	ID          string `json:"experiment_id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

type kfpPipeline struct {
	ID          string `json:"pipeline_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

type kfpRun struct {
	ID         string `json:"run_id"`
	State      string `json:"state"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
	PipelineVersionReference *struct {
		PipelineID string `json:"pipeline_id"`
	} `json:"pipeline_version_reference"`
}

// sampleObjects finds the sample experiment and pipeline (display_name and
// name filters: data-science-pipelines model/experiment.go:54-63,
// model/pipeline.go:78-86). One of the sample's name without its
// description is someone else's.
func (k *kfpTarget) sampleObjects() (*kfpExperiment, *kfpPipeline, error) {
	var exps struct {
		Experiments []kfpExperiment `json:"experiments"`
	}
	if err := k.api.sendJSON("GET", kfpAPI+"/experiments", url.Values{"namespace": {k.project}, "filter": {displayNameFilter("display_name")}, "page_size": {"10"}}, nil, &exps); err != nil {
		return nil, nil, err
	}
	var pipes struct {
		Pipelines []kfpPipeline `json:"pipelines"`
	}
	if err := k.api.sendJSON("GET", kfpAPI+"/pipelines", url.Values{"namespace": {k.project}, "filter": {displayNameFilter("name")}, "page_size": {"10"}}, nil, &pipes); err != nil {
		return nil, nil, err
	}
	var exp *kfpExperiment
	for i, e := range exps.Experiments {
		if e.DisplayName != pipelineSampleName {
			continue
		}
		if e.Description != pipelineSampleDescription {
			return nil, nil, &sampleRefusal{"KFP experiment '" + pipelineSampleName + "' exists but was not created by this tool, so the sample is left alone."}
		}
		exp = &exps.Experiments[i]
	}
	var pipe *kfpPipeline
	for i, p := range pipes.Pipelines {
		if p.Name != pipelineSampleName {
			continue
		}
		if p.Description != pipelineSampleDescription {
			return nil, nil, &sampleRefusal{"Pipeline '" + pipelineSampleName + "' exists but was not created by this tool, so the sample is left alone."}
		}
		pipe = &pipes.Pipelines[i]
	}
	return exp, pipe, nil
}

// experimentRuns lists every run of the experiment, newest first.
func (k *kfpTarget) experimentRuns(experimentID string, limit int) ([]kfpRun, int, error) {
	var all []kfpRun
	total := 0
	token := ""
	for {
		var page struct {
			Runs      []kfpRun `json:"runs"`
			TotalSize int      `json:"total_size"`
			Next      string   `json:"next_page_token"`
		}
		q := url.Values{"namespace": {k.project}, "experiment_id": {experimentID}, "page_size": {"100"}, "sort_by": {"created_at desc"}}
		if token != "" {
			q.Set("page_token", token)
		}
		if err := k.api.sendJSON("GET", kfpAPI+"/runs", q, nil, &page); err != nil {
			return nil, 0, err
		}
		all = append(all, page.Runs...)
		total = page.TotalSize
		if page.Next == "" || (limit > 0 && len(all) >= limit) {
			break
		}
		token = page.Next
	}
	if total < len(all) {
		total = len(all)
	}
	return all, total, nil
}

func (k *kfpTarget) runState(r kfpRun) *types.PipelineSampleRun {
	out := &types.PipelineSampleRun{ID: r.ID, State: r.State, CreatedAt: r.CreatedAt, Link: k.runLink(r.ID)}
	if !strings.HasPrefix(r.FinishedAt, "1970-") {
		out.FinishedAt = r.FinishedAt
	}
	if r.Error != nil {
		out.Error = truncateMessage(r.Error.Message)
	}
	return out
}

// GetPipelineSampleStatus reports the sample pipeline of project: two list
// reads and one run list, cached for pipelineSampleCacheTTL (the UI polls it
// every few seconds while a run is active).
func GetPipelineSampleStatus(c *Client, project string) (*types.PipelineSampleStatus, error) {
	if cached, ok := pipelineSampleCache.Get(project); ok {
		return &cached, nil
	}
	k, reason, err := pipelineSampleTarget(c, project)
	if err != nil {
		return nil, err
	}
	st := types.PipelineSampleStatus{Project: project, Image: pipelineSampleImage()}
	if k == nil {
		st.Reason = reason
		pipelineSampleCache.Add(project, st, pipelineSampleCacheTTL)
		return &st, nil
	}
	st.Available, st.Server = true, dspaName
	if k.dashboard != "" {
		st.RunsLink = k.dashboard + "/develop-train/pipelines/runs/" + project + "/runs"
	}
	exp, pipe, err := k.sampleObjects()
	var refusal *sampleRefusal
	switch {
	case errors.As(err, &refusal):
		st.State, st.Message = "foreign", refusal.msg
	case err != nil:
		st.State, st.Message = "unknown", "Cannot read the sample: "+err.Error()+"."+deniedHint(err)
	case exp == nil && pipe == nil:
		st.State = "absent"
	default:
		st.State = "present"
		if exp != nil {
			runs, total, err := k.experimentRuns(exp.ID, 1)
			if err != nil {
				st.State, st.Message = "unknown", "Cannot list the sample runs: "+err.Error()+"."+deniedHint(err)
				break
			}
			st.Runs = total
			if len(runs) > 0 {
				st.LatestRun = k.runState(runs[0])
			}
		}
	}
	pipelineSampleCache.Add(project, st, pipelineSampleCacheTTL)
	return &st, nil
}

// ValidatePipelineSampleRequest checks a request before the operation
// starts; it returns the problem, or "".
func ValidatePipelineSampleRequest(req types.PipelineSampleRequest) string {
	if req.Action != "run" && req.Action != "remove" {
		return "action must be run or remove"
	}
	return ""
}

// RunPipelineSample starts a run of the sample pipeline, or removes
// everything of the sample, in one project.
func RunPipelineSample(c *Client, req types.PipelineSampleRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}
	emit := func(step, status, msg, code string) {
		logs = append(logs, msg)
		if onStep != nil {
			onStep(UpdateStepEvent{Step: step, Status: status, Message: msg, ErrorCode: code})
		}
	}
	defer InvalidateSamplesStatus(req.Project)
	activity := req.Action + " in " + req.Project
	fail := func(step, msg, code string) (*types.OperationResponse, error) {
		emit(step, "failed", msg, code)
		recordSamplesActivity(c, "pipeline-sample", activity, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	failAPI := func(step, what string, err error) (*types.OperationResponse, error) {
		code := sampleErrorCode(err)
		var refusal *sampleRefusal
		if errors.As(err, &refusal) {
			code = "not_managed"
		}
		return fail(step, fmt.Sprintf("%s: %v.%s", what, err, deniedHint(err)), code)
	}
	if problem := ValidatePipelineSampleRequest(req); problem != "" {
		return fail("check", problem, "validation")
	}
	emit("check", "running", fmt.Sprintf("Checking the pipeline server in '%s'...", req.Project), "")
	ok, err := isDSProject(c, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot list the projects: %v", err), errorCodeFromK8sErr(err))
	}
	if !ok {
		return fail("check", fmt.Sprintf("'%s' is not a Data Science project.", req.Project), "validation")
	}
	k, reason, err := pipelineSampleTarget(c, req.Project)
	if err != nil {
		return fail("check", fmt.Sprintf("Cannot read the pipeline server: %v", err), errorCodeFromK8sErr(err))
	}
	if k == nil {
		return fail("check", reason, "prerequisites")
	}
	exp, pipe, err := k.sampleObjects()
	if err != nil {
		return failAPI("check", "Cannot read the sample", err)
	}
	emit("check", "success", "The pipeline server is ready.", "")

	var resp *types.OperationResponse
	if req.Action == "run" {
		resp, err = k.startSampleRun(exp, pipe, emit, failAPI)
	} else {
		resp, err = k.removeSample(exp, pipe, emit, failAPI)
	}
	if resp != nil && resp.Success {
		resp.Logs = logs
		recordSamplesActivity(c, "pipeline-sample", activity, true)
		slog.Info("pipeline sample", "action", req.Action, "project", req.Project, "user", getUser(c))
	}
	return resp, err
}

type failFunc func(step, what string, err error) (*types.OperationResponse, error)

func (k *kfpTarget) startSampleRun(exp *kfpExperiment, pipe *kfpPipeline, emit func(step, status, msg, code string), fail failFunc) (*types.OperationResponse, error) {
	if exp == nil {
		emit("experiment", "running", "Creating KFP experiment '"+pipelineSampleName+"'...", "")
		var created kfpExperiment
		if err := k.api.sendJSON("POST", kfpAPI+"/experiments", nil, map[string]string{
			"display_name": pipelineSampleName, "description": pipelineSampleDescription, "namespace": k.project,
		}, &created); err != nil {
			return fail("experiment", "Cannot create the experiment", err)
		}
		exp = &created
		emit("experiment", "success", "Created experiment '"+pipelineSampleName+"'", "")
	}
	if pipe == nil {
		emit("pipeline", "running", "Uploading pipeline '"+pipelineSampleName+"' (image "+pipelineSampleImage()+")...", "")
		created, err := k.uploadPipeline()
		if err != nil {
			return fail("pipeline", "Cannot upload the pipeline", err)
		}
		pipe = created
		emit("pipeline", "success", "Uploaded pipeline '"+pipelineSampleName+"'", "")
	}
	var versions struct {
		Versions []struct {
			ID string `json:"pipeline_version_id"`
		} `json:"pipeline_versions"`
	}
	if err := k.api.sendJSON("GET", kfpAPI+"/pipelines/"+url.PathEscape(pipe.ID)+"/versions", url.Values{"page_size": {"1"}, "sort_by": {"created_at desc"}}, nil, &versions); err != nil {
		return fail("run", "Cannot read the pipeline's version", err)
	}
	if len(versions.Versions) == 0 {
		return fail("run", "The sample pipeline has no version", fmt.Errorf("remove the sample and run it again"))
	}
	emit("run", "running", "Starting a run...", "")
	var run kfpRun
	if err := k.api.sendJSON("POST", kfpAPI+"/runs", nil, map[string]interface{}{
		"display_name":  "Sample: write and read an artifact",
		"description":   pipelineSampleDescription,
		"experiment_id": exp.ID,
		"pipeline_version_reference": map[string]string{
			"pipeline_id": pipe.ID, "pipeline_version_id": versions.Versions[0].ID,
		},
	}, &run); err != nil {
		return fail("run", "Cannot start the run", err)
	}
	link := k.runLink(run.ID)
	msg := "Started run " + run.ID
	if link != "" {
		msg += ": " + link
	}
	emit("run", "success", msg, "")
	return &types.OperationResponse{Success: true, Message: "Sample pipeline run started. It usually finishes in 1-2 minutes."}, nil
}

// uploadPipeline uploads pipelineSampleSpec as a new pipeline: multipart
// field "uploadfile" with a .yaml file name, the names and description as
// query parameters (pipeline_upload_server.go:44-52, 96-160).
func (k *kfpTarget) uploadPipeline() (*kfpPipeline, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("uploadfile", pipelineSampleName+".yaml")
	if err != nil {
		return nil, err
	}
	if _, err := part.Write([]byte(pipelineSampleSpec(pipelineSampleImage()))); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	q := url.Values{"name": {pipelineSampleName}, "display_name": {pipelineSampleName}, "description": {pipelineSampleDescription}, "namespace": {k.project}}
	var out kfpPipeline
	if err := k.api.send("POST", kfpAPI+"/pipelines/upload", q, mw.FormDataContentType(), buf.Bytes(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (k *kfpTarget) removeSample(exp *kfpExperiment, pipe *kfpPipeline, emit func(step, status, msg, code string), fail failFunc) (*types.OperationResponse, error) {
	if exp == nil && pipe == nil {
		emit("remove", "success", "Nothing to remove.", "")
		return &types.OperationResponse{Success: true, Message: "The sample pipeline is not in this project; nothing to remove."}, nil
	}
	foreignRuns := 0
	if exp != nil {
		runs, _, err := k.experimentRuns(exp.ID, 0)
		if err != nil {
			return fail("runs", "Cannot list the sample runs", err)
		}
		deleted := 0
		for _, r := range runs {
			if pipe == nil || r.PipelineVersionReference == nil || r.PipelineVersionReference.PipelineID != pipe.ID {
				foreignRuns++
				continue
			}
			if err := k.api.sendJSON("DELETE", kfpAPI+"/runs/"+url.PathEscape(r.ID), nil, nil, nil); err != nil && !kfpNotFound(err) {
				return fail("runs", "Cannot delete run "+r.ID, err)
			}
			deleted++
		}
		emit("runs", "success", fmt.Sprintf("Deleted %d %s", deleted, verb(deleted, "run", "runs")), "")
	}
	if pipe != nil {
		var versions struct {
			Versions []struct {
				ID string `json:"pipeline_version_id"`
			} `json:"pipeline_versions"`
		}
		if err := k.api.sendJSON("GET", kfpAPI+"/pipelines/"+url.PathEscape(pipe.ID)+"/versions", url.Values{"page_size": {"100"}}, nil, &versions); err != nil && !kfpNotFound(err) {
			return fail("pipeline", "Cannot list the pipeline's versions", err)
		}
		for _, v := range versions.Versions {
			if err := k.api.sendJSON("DELETE", kfpAPI+"/pipelines/"+url.PathEscape(pipe.ID)+"/versions/"+url.PathEscape(v.ID), nil, nil, nil); err != nil && !kfpNotFound(err) {
				return fail("pipeline", "Cannot delete a pipeline version", err)
			}
		}
		if err := k.api.sendJSON("DELETE", kfpAPI+"/pipelines/"+url.PathEscape(pipe.ID), nil, nil, nil); err != nil && !kfpNotFound(err) {
			return fail("pipeline", "Cannot delete the pipeline", err)
		}
		emit("pipeline", "success", "Deleted pipeline '"+pipelineSampleName+"'", "")
	}
	msg := "Sample pipeline removed. Its artifacts stay in the S3 storage until the storage is torn down."
	if exp != nil {
		if foreignRuns > 0 {
			emit("experiment", "skipped", fmt.Sprintf("Kept experiment '%s': it holds %d %s this tool did not start.", pipelineSampleName, foreignRuns, verb(foreignRuns, "run", "runs")), "")
			msg += " The experiment was kept because it holds runs the tool did not start."
		} else {
			if err := k.api.sendJSON("DELETE", kfpAPI+"/experiments/"+url.PathEscape(exp.ID), nil, nil, nil); err != nil && !kfpNotFound(err) {
				return fail("experiment", "Cannot delete the experiment", err)
			}
			emit("experiment", "success", "Deleted experiment '"+pipelineSampleName+"'", "")
		}
	}
	return &types.OperationResponse{Success: true, Message: msg}, nil
}
