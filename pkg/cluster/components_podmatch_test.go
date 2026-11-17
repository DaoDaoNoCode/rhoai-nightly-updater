package cluster

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestPodMatchesDeployment(t *testing.T) {
	// The overlap seen live: trustyai-operator-module-controller-manager
	// selects only control-plane=controller-manager, a label other
	// operators' pods carry too.
	trusty := types.DeploymentInfo{Name: "trustyai-operator-module-controller-manager", MatchLabels: map[string]string{"control-plane": "controller-manager"}}
	feast := types.DeploymentInfo{Name: "feast-operator-controller-manager", MatchLabels: map[string]string{"control-plane": "controller-manager", "app.kubernetes.io/name": "feast-operator"}}
	pod := func(owner, kind, hash string, labels map[string]string) types.PodInfo {
		l := map[string]string{}
		for k, v := range labels {
			l[k] = v
		}
		if hash != "" {
			l["pod-template-hash"] = hash
		}
		return types.PodInfo{Labels: l, OwnerKind: kind, OwnerName: owner}
	}
	cm := map[string]string{"control-plane": "controller-manager"}
	feastLabels := map[string]string{"control-plane": "controller-manager", "app.kubernetes.io/name": "feast-operator"}

	for _, tc := range []struct {
		name string
		pod  types.PodInfo
		dep  types.DeploymentInfo
		want bool
	}{
		{"own pod", pod("trustyai-operator-module-controller-manager-b57d5b597", "ReplicaSet", "b57d5b597", cm), trusty, true},
		{"own pod from an older ReplicaSet (rollout)", pod("trustyai-operator-module-controller-manager-6d9f8", "ReplicaSet", "6d9f8", cm), trusty, true},
		{"another operator's pod matching the broad selector", pod("feast-operator-controller-manager-846574fbc4", "ReplicaSet", "846574fbc4", feastLabels), trusty, false},
		{"that pod still belongs to its own Deployment", pod("feast-operator-controller-manager-846574fbc4", "ReplicaSet", "846574fbc4", feastLabels), feast, true},
		{"selector does not match", pod("trustyai-operator-module-controller-manager-b57d5b597", "ReplicaSet", "b57d5b597", map[string]string{"app": "x"}), trusty, false},
		{"owner name matches but hash label differs", pod("trustyai-operator-module-controller-manager-b57d5b597", "ReplicaSet", "zzz", cm), trusty, false},
		{"no pod-template-hash label", pod("trustyai-operator-module-controller-manager-", "ReplicaSet", "", cm), trusty, false},
		{"Job pod with the same labels", pod("trustyai-operator-module-controller-manager-b57d5b597", "Job", "b57d5b597", cm), trusty, false},
		{"bare pod without an owner", pod("", "", "b57d5b597", cm), trusty, false},
		{"deployment whose name is a prefix of another's", pod("feast-operator-controller-manager-846574fbc4", "ReplicaSet", "846574fbc4", feastLabels), types.DeploymentInfo{Name: "feast-operator", MatchLabels: map[string]string{"control-plane": "controller-manager"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podMatchesDeployment(nil, tc.pod, tc.dep); got != tc.want {
				t.Errorf("podMatchesDeployment = %v, want %v", got, tc.want)
			}
		})
	}
}
