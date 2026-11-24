package dashdev

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Bounds of WaitDeploymentsReady: a new dashboard build usually rolls out
// in 1 to 2 minutes; 5 minutes leaves room for a slow image pull. Tests set
// them with SetDeploymentsReadyTimingForTest.
var (
	DeploymentsReadyTimeout = 5 * time.Minute
	deploymentsReadyPoll    = 5 * time.Second
)

var (
	// ErrDeploymentsNotReady: the dashboard Deployments did not roll out
	// within DeploymentsReadyTimeout; they may still.
	ErrDeploymentsNotReady = errors.New("the dashboard deployments are not ready")
	// ErrDeploymentsFailed: waiting cannot help: a Deployment's rollout
	// failed (see rolloutFailure), Dashboard Dev is no longer active (it
	// was reverted, or the operator resumed), or the cluster has no
	// dashboard module.
	ErrDeploymentsFailed = errors.New("the dashboard deployments cannot become ready")
)

// The Deployment conditions the Deployment controller sets when a rollout
// cannot go on by itself (k8s.io/api apps/v1 DeploymentConditionType;
// pkg/controller/deployment/util/deployment_util.go TimedOutReason).
const (
	deploymentProgressing    = "Progressing"
	deploymentReplicaFailure = "ReplicaFailure"
	progressDeadlineExceeded = "ProgressDeadlineExceeded"
)

// WaitDeploymentsReady polls the Deployments that run the dashboard's build
// containers (DiscoverImages) until every one is rolled out and ready
// (DeploymentReady), after a Dashboard Dev deploy (DeployBuild). It
// returns the containers as last read, and:
//
//   - nil once every Deployment is ready;
//   - an error wrapping ErrDeploymentsFailed, with the reason, as soon as
//     waiting cannot help: a Deployment's rollout failed by the Deployment
//     controller's own judgement (Progressing=False with
//     ProgressDeadlineExceeded, or ReplicaFailure=True, for its current
//     generation), Dashboard Dev is no longer active (no session on the
//     operator, or the operator was scaled up), or there is no dashboard
//     module operator;
//   - an error wrapping ErrDeploymentsNotReady, naming the Deployments that
//     are not ready, after DeploymentsReadyTimeout;
//   - the client context's error when that ends first, and any read error
//     but a network error (retried until the timeout).
//
// A pod that cannot pull its image shows only as a Deployment that is not
// ready until the Deployment's progress deadline; the timeout names it.
// Every request runs under one context bounded by the timeout, so a request
// that starts just before the deadline cannot overrun it.
func WaitDeploymentsReady(c *kube.Client) ([]types.DashboardDevImage, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), DeploymentsReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	var images []types.DashboardDevImage
	last := "not read yet"
	for {
		done, err := deploymentsReadyOnce(cc, &images, &last)
		if ctx.Err() != nil {
			if cerr := c.Context().Err(); cerr != nil {
				return images, cerr
			}
			return images, fmt.Errorf("%w after %s: %s", ErrDeploymentsNotReady, DeploymentsReadyTimeout, last)
		}
		switch {
		case kube.IsNetworkError(err):
			last = err.Error()
		case err != nil:
			return images, err
		case done:
			return images, nil
		}
		if kube.Clock().Sleep(ctx, deploymentsReadyPoll) != nil {
			if cerr := c.Context().Err(); cerr != nil {
				return images, cerr
			}
			return images, fmt.Errorf("%w after %s: %s", ErrDeploymentsNotReady, DeploymentsReadyTimeout, last)
		}
	}
}

// deploymentsReadyOnce is one poll of WaitDeploymentsReady: it reports
// whether every Deployment is ready, updating images and last (what is not
// ready yet), or the error that ends the wait (wrapping
// ErrDeploymentsFailed when waiting cannot help).
func deploymentsReadyOnce(c *kube.Client, images *[]types.DashboardDevImage, last *string) (bool, error) {
	_, operator, err := LoadOperator(c, false)
	if err != nil {
		return false, err
	}
	if operator == nil {
		return false, fmt.Errorf("%w: this cluster has no dashboard module operator", ErrDeploymentsFailed)
	}
	session, err := SessionOf(operator)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrDeploymentsFailed, err)
	}
	if session == nil || dashboardOperatorReplicas(operator) != 0 {
		return false, fmt.Errorf("%w: Dashboard Dev is no longer active (%s was resumed or reverted)", ErrDeploymentsFailed, operator.Metadata.Name)
	}
	found, workloads, err := discoverDashboardWorkloads(c, operator)
	if err != nil {
		return false, err
	}
	*images = found
	var waiting []string
	for _, name := range deploymentNames(found) {
		d := workloads[name]
		if DeploymentReady(d) {
			continue
		}
		if why := rolloutFailure(d); why != "" {
			return false, fmt.Errorf("%w: Deployment %s: %s", ErrDeploymentsFailed, name, why)
		}
		replicas := dashboardOperatorReplicas(&d)
		waiting = append(waiting, fmt.Sprintf("%s (%d of %d replicas ready and updated)", name, min(d.Status.ReadyReplicas, d.Status.UpdatedReplicas), replicas))
	}
	if len(waiting) > 0 {
		*last = "not ready: " + strings.Join(waiting, ", ")
		return false, nil
	}
	return true, nil
}

// deploymentNames are the Deployments of images, sorted, once each.
func deploymentNames(images []types.DashboardDevImage) []string {
	seen := map[string]bool{}
	var names []string
	for _, image := range images {
		if !seen[image.Deployment] {
			seen[image.Deployment] = true
			names = append(names, image.Deployment)
		}
	}
	sort.Strings(names)
	return names
}

// rolloutFailure is why the Deployment controller reports d's current
// rollout as failed, or "": Progressing=False with ProgressDeadlineExceeded
// (the rollout made no progress for spec.progressDeadlineSeconds), or
// ReplicaFailure=True (a pod could not be created, for example a quota).
// Conditions of an earlier generation do not count.
func rolloutFailure(d Deployment) string {
	if d.Status.ObservedGeneration < d.Metadata.Generation {
		return ""
	}
	for _, cond := range d.Status.Conditions {
		switch {
		case cond.Type == deploymentProgressing && cond.Status == "False" && cond.Reason == progressDeadlineExceeded,
			cond.Type == deploymentReplicaFailure && cond.Status == "True":
			return kube.FirstNonEmpty(kube.TruncateText(cond.Message, 300), cond.Reason, cond.Type)
		}
	}
	return ""
}

// DeploymentsReadyNow is one poll of WaitDeploymentsReady: whether every
// dashboard Deployment of the Dashboard Dev session is rolled out and
// ready now, what is not yet, or the error that waiting cannot fix
// (wrapping ErrDeploymentsFailed) or a read's.
func DeploymentsReadyNow(c *kube.Client) (ready bool, waiting string, err error) {
	var images []types.DashboardDevImage
	last := ""
	ready, err = deploymentsReadyOnce(c, &images, &last)
	return ready, last, err
}
