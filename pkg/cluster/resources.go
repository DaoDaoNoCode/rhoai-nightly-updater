package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetResourcesStatus checks the state of test infrastructure resources. It
// lists DS projects, pipeline servers (one cluster-wide LIST), S3 storage, MLflow
// and the dashboard route concurrently, so the cost no longer grows with the
// number of projects. Pipeline servers are reported only for projects the
// user can see.
func GetResourcesStatus(c *Client) (*types.ResourcesStatus, error) {
	var (
		wg          sync.WaitGroup
		projects    []string
		projectsErr error
		dspas       []storage.DSPA
		dspasErr    error
		dashURL     string
		s3EP        storage.S3Endpoints
		s3EPErr     error
		legacy      *storage.Legacy
		legacyErr   error
		foreignNS   bool
		dsc         rhoai.DSCRead
		dscErr      error
		models      []serving.LLMISvcObject
		modelsErr   error
		recipeErr   error
		status      = &types.ResourcesStatus{}
	)
	wg.Add(9)
	go func() { defer wg.Done(); models, modelsErr = serving.ListLLMInferenceServices(c) }()
	go func() { defer wg.Done(); projects, projectsErr = envkit.DSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = storage.ListDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO, s3EP, s3EPErr = storage.S3StatusAndEndpoints(c) }()
	go func() { defer wg.Done(); legacy, foreignNS, legacyErr = storage.ReadLegacyOwner(c) }()
	go func() { defer wg.Done(); status.MLflow = mlflow.Status(c) }()
	go func() { defer wg.Done(); dashURL = dashdev.DashboardURL(c) }()
	go func() { defer wg.Done(); dsc, dscErr = rhoai.ReadFirstDSC(c) }()
	go func() { defer wg.Done(); status.RecipeProjects, recipeErr = recipe.OwnedProjects(c) }()
	wg.Wait()
	if projectsErr != nil {
		return nil, projectsErr
	}
	if dspasErr != nil && !errors.Is(dspasErr, storage.ErrDSPACRDMissing) {
		return nil, dspasErr
	}

	// The projects recipes created that exist, for Clean up everything; a
	// list that cannot be read offers none and says why.
	if recipeErr != nil {
		status.RecipeProjects = nil
		status.RecipeProjectsError = "Cannot read the projects recipes created: " + recipeErr.Error()
	}
	// The project storages, from the record only (the ServiceAccount
	// reads none of their objects).
	if rec, err := storage.ReadProjectStorageRecord(c); err != nil {
		status.ProjectStoragesError = "Cannot read this tool's record of project S3 storages: " + err.Error()
	} else {
		for _, p := range rec.Projects() {
			e, _ := rec.Get(p)
			status.ProjectStorages = append(status.ProjectStorages, types.RecordedProjectStorage{Project: p, By: e.By, DataVolume: storage.ProjectS3PVC})
		}
		sort.Slice(status.ProjectStorages, func(i, j int) bool { return status.ProjectStorages[i].Project < status.ProjectStorages[j].Project })
	}
	status.DashboardURL = dashURL
	status.ForeignLegacyNamespace = foreignNS
	// What the features need from the DataScienceCluster. One that cannot
	// be read blocks nothing; setup still checks the CRD.
	if dscErr != nil {
		slog.Debug("cannot read the DataScienceCluster for the Environment page", "error", dscErr)
	} else {
		status.PipelinesBlocked = envkit.ComponentBlock(dsc, envkit.PipelinesComponent)
		status.MLflowBlocked = envkit.ComponentBlock(dsc, envkit.MLflowComponent)
	}
	if status.PipelinesBlocked == nil && errors.Is(dspasErr, storage.ErrDSPACRDMissing) {
		status.PipelinesBlocked = &types.ComponentBlock{Component: envkit.PipelinesComponent.Keys[0],
			Reason: "Data Science Pipelines is not installed yet: this cluster has no DataSciencePipelinesApplication API. If the component was just turned on, wait for the operator; otherwise check the Components page."}
	}

	visible := map[string]bool{}
	for _, p := range projects {
		visible[p] = true
	}
	// Models: the inference gateway (which models use it) and the
	// catalog's models in the visible projects.
	var mwg sync.WaitGroup
	mwg.Add(2)
	go func() {
		defer mwg.Done()
		status.InferenceGateway = serving.InferenceGatewayStatus(c, models, modelsErr)
	}()
	go func() {
		defer mwg.Done()
		status.Models = serving.ModelsStatus(c, dsc, dscErr, models, modelsErr, visible)
	}()
	mwg.Wait()

	// A record that cannot be read leaves every pipeline server unrecorded
	// (fail closed: Migrate storage then asks before moving one).
	pipelineRec, recErr := storage.ReadPipelineRecord(c)
	if recErr != nil {
		slog.Debug("cannot read the pipeline server record", "error", recErr)
		pipelineRec = storage.PipelineRecord{}
	}
	unmanaged := map[string]bool{}
	for _, d := range dspas {
		if !visible[d.Meta.Namespace] {
			// This tool's, in a project that is not (or no longer) a Data
			// Science project: listed apart, so it can still be deleted.
			if d.ManagedByTool() && !d.Meta.Terminating() {
				ps := storage.PipelineServerState(d, dashURL)
				ps.Unrecorded = !pipelineRec.Owns(d.Meta)
				if ps.Unrecorded {
					ps.UID = d.Meta.UID
				}
				status.OutsidePipelineServers = append(status.OutsidePipelineServers, ps)
			}
			continue
		}
		if !d.ManagedByTool() {
			unmanaged[d.Meta.Namespace] = true
			foreign := storage.PipelineServerState(d, dashURL)
			foreign.ManagedByTool = false
			foreign.DataPVCs = nil
			foreign.TeardownBlockedReason = "This pipeline server was not created by this tool, so the tool will not delete it."
			status.UnmanagedPipelineServers = append(status.UnmanagedPipelineServers, foreign)
			continue
		}
		ps := storage.PipelineServerState(d, dashURL)
		ps.Unrecorded = !pipelineRec.Owns(d.Meta)
		if ps.Unrecorded {
			ps.UID = d.Meta.UID
		}
		status.PipelineServers = append(status.PipelineServers, ps)
	}
	for ns := range unmanaged {
		status.UnmanagedPipelineProjects = append(status.UnmanagedPipelineProjects, ns)
	}
	sort.Strings(status.UnmanagedPipelineProjects)
	sort.Slice(status.UnmanagedPipelineServers, func(i, j int) bool {
		a, b := status.UnmanagedPipelineServers[i], status.UnmanagedPipelineServers[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.Slice(status.PipelineServers, func(i, j int) bool {
		return status.PipelineServers[i].Namespace < status.PipelineServers[j].Namespace
	})
	sort.Slice(status.OutsidePipelineServers, func(i, j int) bool {
		return status.OutsidePipelineServers[i].Namespace < status.OutsidePipelineServers[j].Namespace
	})

	// Storage an earlier version left in namespace minio: Migrate storage
	// replaces Set up and Repair while it is there (SetupS3Storage refuses),
	// and teardown removes it with the current storage.
	switch {
	case legacyErr != nil:
		status.MinIO.Warning = strings.TrimSpace(fmt.Sprintf("Cannot check namespace %s for storage from an earlier version: %v. %s", storage.LegacyNamespace, legacyErr, status.MinIO.Warning))
	case legacy != nil:
		newReady := status.MinIO.Ready && status.MinIO.ManagedByTool && status.MinIO.RepairNeeded == ""
		// Unreadable counts as not copied: the stage stays "migrate".
		copied, _ := storage.MigrationCopied(c)
		status.LegacyStorage = storage.LegacyState(legacy, dspas, pipelineRec, newReady, copied)
		status.MinIO.MigrationPending = true
	}

	// Which keys the storage's clients use: on storage an earlier version
	// set up, every pipeline project holds the admin key until Secure S3
	// storage runs (never on its own). Read only when no migration is
	// pending: Migrate storage leaves the storage secured.
	if legacy == nil && legacyErr == nil && s3EPErr == nil {
		status.S3Keys = storage.KeysState(c, status.MinIO, s3EP, dspas, pipelineRec)
	}

	// Also for a storage that is gone but left its data PVC: teardown
	// deletes it under the same guard.
	current := status.MinIO.ManagedByTool && (status.MinIO.Deployed || len(status.MinIO.DataPVCs) > 0)
	if current || legacy != nil {
		var addresses []storage.Address
		var addrErr error
		if current {
			addresses, addrErr = append(addresses, s3EP.Address), s3EPErr
		}
		if legacy != nil {
			addresses = append(addresses, legacy.Address)
			if addrErr == nil {
				addrErr = legacy.AddressErr
			}
		}
		if addrErr != nil && len(dspas) > 0 {
			status.MinIO.TeardownBlockedReason = "Cannot verify which pipeline servers use the S3 storage: " + addrErr.Error()
		} else if reason := storage.S3TeardownBlocker(dspas, addresses...); reason != "" {
			status.MinIO.TeardownBlockedReason = reason
		}
		if addrErr == nil {
			live, deleting := storage.Users(dspas, addresses...)
			status.MinIO.UsedBy = append(live, deleting...)
		}
	}
	return status, nil
}

// What the selected project's Connection Secret says (ResourceState.
// ConnectionState).
const (
	ConnectionPresent    = "present"
	ConnectionMissing    = "missing"
	ConnectionForeign    = "foreign"
	ConnectionUnreadable = "unreadable"
)

// GetResourcesStatusFor is GetResourcesStatus plus, for the tool's pipeline
// server in project (the Pipelines tab's selected project), the state of
// its dashboard Connection read from the Secret itself: the DSPA's
// annotation only says the tool created one, which the dashboard may since
// have deleted. One named GET, only when the project has the tool's
// server (so only in a visible Data Science project); other projects, and
// an empty project, read no Secret. With a present Connection, u (the
// signed-in user's client; nil reads nothing) lists the project's
// workbenches that use it (ResourceState.ConnectionWorkbenches).
func GetResourcesStatusFor(c, u *Client, project string) (*types.ResourcesStatus, error) {
	status, err := GetResourcesStatus(c)
	if err != nil || project == "" {
		return status, err
	}
	for i := range status.PipelineServers {
		ps := &status.PipelineServers[i]
		if ps.Namespace != project || ps.Terminating {
			continue
		}
		// The Connection of a pipeline server on project storage
		// (nightly-s3-connection) holds that project's root key: the
		// ServiceAccount may not read it, so only the signed-in user's
		// own token does.
		if ps.ProjectStorage {
			meta, found, err := readProjectConnection(u, project)
			ps.ConnectionState, ps.ConnectionMessage = connectionStateOf(meta, found, err)
			if ps.ConnectionState == ConnectionPresent {
				addConnectionWorkbenches(u, project, storage.ProjectConnectionSecretName, ps)
			}
			continue
		}
		meta, found, err := kube.ReadObjectMeta(c, rhoai.NamespacedPath("v1", "secrets", project, storage.ConnectionSecretName))
		ps.ConnectionState, ps.ConnectionMessage = connectionStateOf(meta, found, err)
		if ps.ConnectionState == ConnectionPresent && u != nil {
			addConnectionWorkbenches(u, project, storage.ConnectionSecretName, ps)
		}
	}
	return status, nil
}

// connectionStateOf is the ResourceState.ConnectionState of a Connection
// Secret as read.
func connectionStateOf(meta kube.ObjectMeta, found bool, err error) (string, string) {
	switch {
	case err != nil:
		return ConnectionUnreadable, err.Error()
	case !found:
		return ConnectionMissing, ""
	case !meta.HasToolLabel():
		return ConnectionForeign, ""
	}
	return ConnectionPresent, ""
}

// readProjectConnection reads the dashboard Connection of a pipeline
// server on project storage with u, the signed-in user's own token (nil:
// unreadable); never with the ServiceAccount's.
func readProjectConnection(u *Client, project string) (kube.ObjectMeta, bool, error) {
	if u == nil || !u.ActsAsUser() {
		return kube.ObjectMeta{}, false, authz.ErrNotUserClient
	}
	var sec struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	found, err := u.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", project, storage.ProjectConnectionSecretName), &sec)
	return sec.Metadata, found, err
}
