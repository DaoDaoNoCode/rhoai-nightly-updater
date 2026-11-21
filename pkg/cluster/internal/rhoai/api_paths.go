package rhoai

import "fmt"

// NamespacedPath builds a K8s API path for a namespaced resource.
// Example: NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", "ns", "name")
// returns "/apis/operators.coreos.com/v1alpha1/namespaces/ns/subscriptions/name".
func NamespacedPath(apiGroup, resource, namespace, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/namespaces/%s/%s", prefix, apiGroup, namespace, resource)
	}
	return fmt.Sprintf("%s/%s/namespaces/%s/%s/%s", prefix, apiGroup, namespace, resource, name)
}

// ClusterPath builds a K8s API path for a cluster-scoped resource.
// Example: ClusterPath("config.openshift.io/v1", "clusterversions", "version")
// returns "/apis/config.openshift.io/v1/clusterversions/version".
func ClusterPath(apiGroup, resource, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/%s", prefix, apiGroup, resource)
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, apiGroup, resource, name)
}
