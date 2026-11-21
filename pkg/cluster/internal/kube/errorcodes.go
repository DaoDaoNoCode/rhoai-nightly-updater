package kube

// ErrorCodeGitOpsManaged marks a refusal because Argo CD manages the object
// the action would change, and would undo the change.
const ErrorCodeGitOpsManaged = "gitops_managed"

// ErrorCodeProfileGitOps marks a request the gitops profile refuses
// (docs/GITOPS.md, "Profiles"): the route is off in that profile, or Update
// would have to take a path that profile does not have.
const ErrorCodeProfileGitOps = "profile_gitops"
