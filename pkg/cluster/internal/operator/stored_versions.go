package operator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// spaced returns " "+s, or "" for an empty s.
func spaced(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// storedVersionConflicts compares the CRD versions the target bundle lists
// with the live CRDs' status.storedVersions. OLM fails an InstallPlan whose
// CRD drops a version still listed in storedVersions ("risk of data loss",
// operator-lifecycle-manager lib/crd/storage.go; RHOAI operator notes §1.4),
// which a downgrade can hit (live: DSC and DSCI store v2, which 2.x bundles
// lack). Only the operator's own CRDs (OLM package label) are read; CRDs
// the target does not list are skipped.
func storedVersionConflicts(c *kube.Client, owned map[string][]string) ([]string, error) {
	body, _, err := c.Do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"labelSelector": {components.RHOAICRDSelectors[0]}})
	if err != nil {
		return nil, fmt.Errorf("list the operator's CRDs: %w", err)
	}
	var list struct {
		Items []components.ConversionCRD `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse CRDs: %w", err)
	}
	var out []string
	for _, crd := range list.Items {
		versions, listed := owned[crd.Metadata.Name]
		if !listed {
			continue
		}
		for _, stored := range crd.Status.StoredVersions {
			if !slices.Contains(versions, stored) {
				out = append(out, fmt.Sprintf("CRD %s stores objects as %s, which the target lists only as %s", crd.Metadata.Name, stored, strings.Join(versions, ", ")))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
