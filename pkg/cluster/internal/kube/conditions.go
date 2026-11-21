package kube

// Condition is one entry of an object's status.conditions.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// FindCondition returns the condition of type typ, and whether there is one.
func FindCondition(conds []Condition, typ string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return Condition{}, false
}
