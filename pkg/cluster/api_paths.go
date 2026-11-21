package cluster

import "fmt"

// namespacedPath builds a K8s API path for a namespaced resource.
// Example: namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", "ns", "name")
// returns "/apis/operators.coreos.com/v1alpha1/namespaces/ns/subscriptions/name".
func namespacedPath(apiGroup, resource, namespace, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/namespaces/%s/%s", prefix, apiGroup, namespace, resource)
	}
	return fmt.Sprintf("%s/%s/namespaces/%s/%s/%s", prefix, apiGroup, namespace, resource, name)
}

// clusterPath builds a K8s API path for a cluster-scoped resource.
// Example: clusterPath("config.openshift.io/v1", "clusterversions", "version")
// returns "/apis/config.openshift.io/v1/clusterversions/version".
func clusterPath(apiGroup, resource, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/%s", prefix, apiGroup, resource)
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, apiGroup, resource, name)
}
