package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Webhook configurations related to RHOAI come from two places (live cluster,
// RHOAI 3.6):
//   - OLM creates one per CSV webhookdefinition, labelled olm.owner=<csv> and
//     olm.owner.namespace=redhat-ods-operator, served by rhods-operator-service.
//     OLM's operators.coreos.com/csv-cleanup finalizer deletes them when that
//     CSV is deleted, and the next CSV recreates them (operator-lifecycle-manager
//     pkg/controller/operators/olm/operator.go, processFinalizer).
//   - Component controllers create operand webhooks (KServe, Workbenches, ...)
//     with an ownerReference to their component CR, served from
//     redhat-ods-applications. These keep serving while the operator is
//     reinstalled and must not be removed while their Service exists.
//
// A configuration is only deleted when it can do nothing but fail requests
// and nothing will fix it (RHOAI_OPERATOR_NOTES §3.4):
//   - OLM-owned: its CSV is gone. While the CSV exists OLM heals the configs
//     itself, so they are never touched.
//   - Runtime/operand: a Service it calls is NotFound and no RHOAI CSV is
//     being installed. A Service without ready endpoints is not treated as
//     stale (a restarting pod is not staleness).

// isRHOAIWebhook reports whether a webhook configuration was installed by OLM
// for the RHOAI operator CSV, or carries an RHOAI name without any owner
// label (a leftover from older installs). Callers that delete must also check
// that the configuration is stale; see staleRHOAIWebhooks.
func isRHOAIWebhook(name string, labels map[string]interface{}) bool {
	if rhoaiOLMOwner(labels) != "" {
		return true
	}
	if owner, _ := labels["olm.owner"].(string); owner != "" {
		// Owned by a different operator's CSV.
		return false
	}
	return strings.Contains(name, "opendatahub") || strings.Contains(name, "rhods")
}

// rhoaiOLMOwner returns the RHOAI CSV named in the olm.owner label, or "".
func rhoaiOLMOwner(labels map[string]interface{}) string {
	owner, _ := labels["olm.owner"].(string)
	if owner != SubName && !strings.HasPrefix(owner, SubName+".") {
		return ""
	}
	if ns, _ := labels["olm.owner.namespace"].(string); ns != "" && ns != SubNS {
		return ""
	}
	return owner
}

// ownedByRHOAIComponent reports whether any ownerReference points at an
// RHOAI API group (e.g. components.platform.opendatahub.io Kserve).
func ownedByRHOAIComponent(meta map[string]interface{}) bool {
	refs, _ := meta["ownerReferences"].([]interface{})
	for _, ref := range refs {
		r, _ := ref.(map[string]interface{})
		apiVersion, _ := r["apiVersion"].(string)
		group := strings.SplitN(apiVersion, "/", 2)[0]
		if strings.HasSuffix(group, ".opendatahub.io") {
			return true
		}
	}
	return false
}

type staleWebhook struct {
	path   string
	kind   string
	name   string
	reason string
}

// staleRHOAIWebhooks lists RHOAI webhook configurations that can only fail
// requests. Live configurations are never returned, whoever owns them.
func staleRHOAIWebhooks(c *Client) ([]staleWebhook, []string) {
	var stale []staleWebhook
	var warnings []string
	csvGone := map[string]bool{}
	serviceMissing := map[string]bool{}
	var installing *bool

	for _, kind := range []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"} {
		listPath := clusterPath("admissionregistration.k8s.io/v1", kind, "")
		body, _, err := c.get(listPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to list %s: %v", kind, err))
			continue
		}
		var list struct {
			Items []map[string]interface{} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to parse %s: %v", kind, err))
			continue
		}
		for _, obj := range list.Items {
			meta, _ := obj["metadata"].(map[string]interface{})
			name, _ := meta["name"].(string)
			labels, _ := meta["labels"].(map[string]interface{})
			if name == "" || (!isRHOAIWebhook(name, labels) && !ownedByRHOAIComponent(meta)) {
				continue
			}
			reason := ""
			if owner := rhoaiOLMOwner(labels); owner != "" {
				gone, known := csvGone[owner]
				if !known {
					gone, err = rhoaiCSVGone(c, owner)
					if err != nil {
						warnings = append(warnings, fmt.Sprintf("cannot check owner %s of %s: %v", owner, name, err))
						continue
					}
					csvGone[owner] = gone
				}
				if !gone {
					// OLM recreates and heals the configs of an existing CSV.
					continue
				}
				reason = "its operator CSV " + owner + " was removed"
			} else {
				if installing == nil {
					busy, err := rhoaiCSVInstalling(c)
					if err != nil {
						warnings = append(warnings, fmt.Sprintf("cannot check whether the operator is being installed: %v", err))
						busy = true
					}
					installing = &busy
				}
				if *installing {
					// An install in progress re-applies these; do not race it.
					continue
				}
				svc, missing, svcErr := missingWebhookService(c, obj, serviceMissing)
				if svcErr != nil {
					warnings = append(warnings, fmt.Sprintf("cannot check the Service of %s: %v", name, svcErr))
					continue
				}
				if missing {
					reason = "its Service " + svc + " does not exist"
				}
			}
			if reason != "" {
				stale = append(stale, staleWebhook{path: listPath + "/" + name, kind: kind, name: name, reason: reason})
			}
		}
	}
	return stale, warnings
}

// rhoaiCSVInstalling reports whether an RHOAI CSV is mid-install (Pending,
// InstallReady, Installing or Replacing); runtime webhooks are then about to
// be re-applied and must not be touched.
func rhoaiCSVInstalling(c *Client) (bool, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if IsK8sError(err, 404) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, err
	}
	for _, item := range list.Items {
		if !strings.HasPrefix(item.Metadata.Name, SubName+".") {
			continue
		}
		switch item.Status.Phase {
		case "Pending", "InstallReady", "Installing", "Replacing":
			return true, nil
		}
	}
	return false, nil
}

// rhoaiCSVGone reports whether the CSV no longer exists or is being deleted.
func rhoaiCSVGone(c *Client, name string) (bool, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, name))
	if IsK8sError(err, 404) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var csv struct {
		Metadata struct {
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &csv); err != nil {
		return false, err
	}
	return csv.Metadata.DeletionTimestamp != nil, nil
}

// missingWebhookService returns the first Service referenced by the
// configuration that does not exist. Errors other than 404 are returned so
// the caller keeps the configuration rather than guessing.
func missingWebhookService(c *Client, obj map[string]interface{}, cache map[string]bool) (string, bool, error) {
	webhooks, _ := obj["webhooks"].([]interface{})
	for _, wh := range webhooks {
		whMap, _ := wh.(map[string]interface{})
		clientConfig, _ := whMap["clientConfig"].(map[string]interface{})
		svcRef, _ := clientConfig["service"].(map[string]interface{})
		svcName, _ := svcRef["name"].(string)
		svcNS, _ := svcRef["namespace"].(string)
		if svcName == "" || svcNS == "" {
			continue
		}
		key := svcNS + "/" + svcName
		missing, known := cache[key]
		if !known {
			_, _, err := c.get(namespacedPath("v1", "services", svcNS, svcName))
			switch {
			case IsK8sError(err, 404):
				missing = true
			case err != nil:
				return key, false, err
			}
			cache[key] = missing
		}
		if missing {
			return key, true, nil
		}
	}
	return "", false, nil
}

// removeStaleWebhooks deletes the configurations from staleRHOAIWebhooks and
// describes each deletion. It never deletes a configuration whose owner CSV
// and Services are still present.
func removeStaleWebhooks(c *Client) ([]string, []string) {
	stale, warnings := staleRHOAIWebhooks(c)
	var removed []string
	for _, wh := range stale {
		kind := strings.TrimSuffix(wh.kind, "s")
		if _, err := c.delete(wh.path); err != nil && !IsK8sError(err, 404) {
			warnings = append(warnings, fmt.Sprintf("failed to delete %s %s: %v", kind, wh.name, err))
			continue
		}
		removed = append(removed, fmt.Sprintf("%s %s (%s)", kind, wh.name, wh.reason))
	}
	return removed, warnings
}

// cleanupStaleWebhooks is removeStaleWebhooks returning only the count.
func cleanupStaleWebhooks(c *Client) (int, []string) {
	removed, warnings := removeStaleWebhooks(c)
	return len(removed), warnings
}
