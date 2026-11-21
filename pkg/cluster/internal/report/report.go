// Package report holds the diagnostics report types: problems, check
// results and the plans of their fixes. Problem's Explains, Mergeable and
// Covers link problems within one scan and are never sent (json:"-").
package report

// CheckResult reports the outcome of a single diagnostic check.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "pass", "fail", "warn"
	Detail string `json:"detail"`
}

// Problem describes a detected issue with plain-English guidance.
type Problem struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"` // "critical", "warning", "info"
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Evidence       []string `json:"evidence,omitempty"`
	Fix            string   `json:"fix,omitempty"`
	AutoFixable    bool     `json:"autoFixable"`
	AutoFixAction  string   `json:"autoFixAction,omitempty"`
	ConfirmMessage string   `json:"confirmMessage,omitempty"`
	// AffectedObjects lists the cluster objects the problem is about, as
	// "<Kind> <namespace>/<name>" or "<Kind> <name>". For auto-fixable
	// problems these are exactly the objects the fix may change.
	AffectedObjects []string `json:"affectedObjects,omitempty"`
	LearnMore       string   `json:"learnMore,omitempty"`
	TechnicalCmd    string   `json:"technicalCmd,omitempty"`
	// RelatedProblems are the IDs of other problems in the same report that
	// fix this problem's cause (only IDs present in the report are kept).
	RelatedProblems []string `json:"relatedProblems,omitempty"`
	// FieldChanges is the per-field plan of a fix that changes fields of
	// an object: the value now, the value after, and why.
	FieldChanges []FieldChange `json:"fieldChanges,omitempty"`
	// Acknowledge, when set, is a statement the user must confirm before
	// the fix runs: the fix is sent to /api/diagnostics/fix/acknowledged,
	// and refused without it.
	Acknowledge string `json:"acknowledge,omitempty"`
	// AcknowledgeBinding is set with Acknowledge: what the statement is
	// about, sent back with the acknowledged fix. The fix refuses
	// ("conflict") unless the object it would change still has this UID
	// and resourceVersion and the statement it would ask for now hashes
	// the same, so an acknowledgement never covers an object or reasons
	// the user did not review.
	AcknowledgeBinding *AckBinding `json:"acknowledgeBinding,omitempty"`

	// Explains are objects (pods) whose problems this one likely causes:
	// a pod problem whose objects are all listed links to this problem.
	Explains []string `json:"-"`

	// Mergeable: two checks may report the same problem (a prerequisite
	// named by a module and needed by a Certificate); their evidence is
	// merged instead of the second being dropped.
	Mergeable bool `json:"-"`
	// Covers are objects whose own problems this one explains, as
	// "<Kind> <namespace>/<name>" or "<Kind> <name>". Pod problems whose
	// pods are all covered, and webhook entries covered here, are folded
	// into this problem.
	Covers []string `json:"-"`
}

// AckBinding ties an acknowledgement to what the user reviewed: the UID and
// resourceVersion of the object the fix changes (both empty when the scan
// could not read it, and the statement says so) and the SHA-256 of the
// acknowledgement statement, which lists the reasons.
type AckBinding struct {
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Hash            string `json:"hash"`
}

// FieldChange is one field of a fix's plan.
type FieldChange struct {
	Object string `json:"object"` // "<Kind> <name>"
	Field  string `json:"field"`  // ".spec.eval.lmeval.permitOnline"
	Now    string `json:"now"`    // the stored value, as JSON
	After  string `json:"after"`  // the value after the fix, as JSON; "" when removed or not changed
	// Change is "set-default", "remove", "none" (no automatic fix) or
	// "valid" (already matches the CRD).
	Change string `json:"change"`
	Schema string `json:"schema"` // what the installed CRD says about the field
	Note   string `json:"note,omitempty"`
	// Review: the stored value may not mean the same as the new one, so
	// the fix needs an acknowledgement.
	Review bool `json:"review,omitempty"`
}

// DiagnosticsResponse is the response for the diagnostics endpoint.
type DiagnosticsResponse struct {
	Problems []Problem     `json:"problems"`
	Checks   []CheckResult `json:"checks"`
}
