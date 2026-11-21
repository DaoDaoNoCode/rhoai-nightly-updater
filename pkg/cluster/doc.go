// Package cluster is what pkg/api and main.go call to read and change the
// cluster. It is a facade over the packages under internal/, plus the
// orchestrators that combine several of them (status, summary, resources,
// the components page, the user-token project reads, clean-up).
//
// The facade_<package>.go files re-export internal/<package> under the names
// pkg/api uses: type aliases, constants, error values (the same values, so
// errors.Is matches either name) and functions. A facade function is a pure
// forward, `return pkg.F(its parameters in order)`, nothing else: the RBAC
// scan (rbac_coverage_test.go) follows a parameter through a forward, and a
// wrapper that computes, reorders or drops an argument is an ordinary caller
// to it.
//
// The internal packages, each importing only packages listed above it:
//
//	kube, report            the API client and its errors, REST helpers, object
//	                        and text helpers, the clock, the cache registry;
//	                        the diagnostics report types
//	rhoai, authz            the product's namespaces, API versions and
//	                        platform readers; the user-token checks
//	record, argo, registry  the updater's own records; the Argo CD and ACM
//	                        guard; Quay, FBC and GitHub reads
//	catalog, components     catalog verification and the stable target; the
//	                        DSC model and the component health readers
//	dashdev, envkit, node   Dashboard Dev; ownership and operator installs;
//	                        node setup and the node pull check
//	mlflow, storage,        MLflow and its samples; S3 storage and pipeline
//	operator                servers; Update, Reinstall and Refresh
//	serving, gitops         models, the inference gateway and MaaS; the gitops
//	                        profile's update and admission-policy check
//	dsc                     the DataScienceCluster's writes and their guards
//	features, diagnostics   feature setups and the playground; the
//	                        diagnostics runner, its checks and fixes
//
// No internal package imports pkg/cluster. layers_test.go checks both rules
// against its own copy of this list, and that this list and CONTRIBUTING.md
// agree with it. Most tests stay in this package (they share its fake
// clusters) and reach the internal packages directly; the static tests
// (rbac_coverage_test.go, rbac_gitops_test.go, cache_isolation_test.go,
// layers_test.go) read every package under pkg/cluster.
package cluster
