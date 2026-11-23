package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Cluster state—not the run record—holds what a run owns. For S3 storage,
// pipeline server, or MLflow setup steps, the object is inspected before
// and after the feature's Set up; the step stamps
// kube.RecipeRunAnnotation with the run's ID only when it was missing
// beforehand and present afterward (the run brought it into existence).
// Ownership surfaces resources that still bear that stamp; during Clean up
// the run re-checks before each removal (storage.TeardownS3OfRun,
// TeardownPipelineServerOfRun, mlflow.TeardownOfRun).

// runFieldManager names the field manager for the run annotation—not the
// tool's—so a later Set up via the tool's server-side apply does not strip
// the annotation from the object.
const runFieldManager = "rhoai-nightly-updater-recipe"

// runMarkReadTimeout limits post–Set up reads and annotation work even
// after the step context has been cancelled.
const runMarkReadTimeout = 10 * time.Second

// storagePath, dspaPath, and mlflowPath identify markable objects; the
// SeaweedFS Deployment represents S3 storage.
func storagePath() string {
	return rhoai.NamespacedPath("apps/v1", "deployments", storage.S3Namespace, storage.S3Deployment)
}

func dspaPath(project string) string {
	return rhoai.NamespacedPath(storage.DSPAAPIGroup, "datasciencepipelinesapplications", project, storage.DSPAName)
}

func mlflowPath() string { return rhoai.ClusterPath(mlflow.APIGroup, "mlflows", mlflow.CRName) }

// setUpAndMark invokes setup, then tags path for the run when setup
// materialized the object (missing beforehand per the tool's result). Failed
// reads or marks claim nothing; the step message reflects that (fail closed:
// Clean up for the run omits it; Environment-page Clean up everything may
// still remove it).
func setUpAndMark(c *kube.Client, s *State, what, path string, setup func() (*types.OperationResponse, error)) (*types.OperationResponse, error) {
	_, existed, beforeErr := kube.ReadObjectMeta(c, path)
	res, err := setup()
	if beforeErr == nil && existed {
		return res, err // already present: not attributed to this run
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), runMarkReadTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	markErr := beforeErr
	if markErr == nil {
		meta, found, readErr := kube.ReadObjectMeta(cc, path)
		switch {
		case readErr != nil:
			markErr = readErr
		case !found || !meta.HasToolLabel():
			return res, err // setup did not produce a markable object
		default:
			markErr = markRun(cc, path, meta.UID, s.RunID)
		}
	}
	if markErr != nil {
		note := fmt.Sprintf("The run could not mark %s as its own (%v), so the recipe's Clean up does not offer it; Clean up everything on the Environment page does.", what, markErr)
		slog.Warn("recipe step: not marked as the run's", "what", what, "error", markErr)
		if res != nil {
			res.Message = strings.TrimSpace(res.Message + " " + note)
			res.Logs = append(res.Logs, "WARN: "+note)
		}
	}
	return res, err
}

// markRun applies the run annotation at path while uid still matches that
// object: JSON patch tests UID and sends the resourceVersion from the read
// (409 if another writer changed it—re-read, at most labelPatchAttempts).
func markRun(c *kube.Client, path, uid, run string) error {
	if run == "" {
		return fmt.Errorf("the run has no ID")
	}
	rawUID, _ := json.Marshal(uid)
	var err error
	for range labelPatchAttempts {
		var (
			meta  kube.ObjectMeta
			found bool
		)
		meta, found, err = kube.ReadObjectMeta(c, path)
		switch {
		case err != nil:
			return err
		case !found || meta.UID != uid:
			return fmt.Errorf("it was replaced right after it was created")
		}
		rawRV, _ := json.Marshal(meta.ResourceVersion)
		ops := []kube.JSONPatchOp{{Op: "test", Path: "/metadata/uid", Value: rawUID}, {Op: "replace", Path: "/metadata/resourceVersion", Value: rawRV}}
		ops = append(ops, setMapOps("/metadata/annotations", meta.Annotations == nil, map[string]string{kube.RecipeRunAnnotation: run})...)
		patch, mErr := json.Marshal(ops)
		if mErr != nil {
			return mErr
		}
		_, _, err = c.Do(http.MethodPatch, path, "application/json-patch+json", patch, url.Values{"fieldManager": {runFieldManager}})
		if !kube.IsK8sConflict(err) {
			return err
		}
	}
	return err
}

// ofRun is true when path points to a tool-labeled object whose run
// annotation matches run; any read error yields false.
func ofRun(c *kube.Client, path, run string) bool {
	meta, found, err := kube.ReadObjectMeta(c, path)
	return err == nil && found && meta.HasToolLabel() && meta.OfRun(run)
}

// dspaOfRun reports whether project's pipeline server of the tool's name is
// the tool's and carries run's annotation. It lists (what the
// ServiceAccount may do in any project, for the status) rather than reads
// it by name; a list that fails is no.
func dspaOfRun(c *kube.Client, project, run string) bool {
	dspas, err := storage.ListDSPAs(c, project)
	if err != nil {
		return false
	}
	for _, d := range dspas {
		if d.Meta.Name == storage.DSPAName && d.ManagedByTool() && d.Meta.OfRun(run) {
			return true
		}
	}
	return false
}
