package cluster

import (
	"fmt"
	"strings"
)

// deleteInfraNamespace deletes the infrastructure namespace setup created,
// only when it is still the tool's, MaaS is off (RHOAI's operator does not
// use it), and nothing else this tool can see is in it (fail closed: a read
// that fails keeps it). It returns why it was kept ("" when deleted or
// gone); the record keeps the namespace while it stays, so a later Remove
// tries again. The error is a failed record write.
func (m *maasRun) deleteInfraNamespace(rec *maasRecord, ns string) (string, error) {
	c := m.c
	forget := func() error {
		rec.InfraNamespace = ""
		return writeMaaSRecord(c, *rec)
	}
	path, ok := maasInfraNamespacePath(ns)
	if !ok {
		return "", forget()
	}
	meta, found, err := readObjectMeta(c, path)
	switch {
	case err != nil:
		return fmt.Sprintf("Kept namespace %s, which this tool created: it cannot be read (%v). Remove again to retry.", ns, err), nil
	case !found:
		return "", forget()
	case !toolOwned(meta):
		return fmt.Sprintf("Kept namespace %s: it no longer carries this tool's label.", ns), forget()
	case meta.terminating():
		return "", forget()
	}
	if dsc, err := readMaaSDSC(c); err != nil || dsc.maas == "Managed" {
		return fmt.Sprintf("Kept namespace %s, which this tool created: MaaS is still on in the DataScienceCluster (or it cannot be read), and RHOAI's operator uses the namespace then.", ns), nil
	}
	left, err := infraNamespaceLeftovers(c, ns)
	if err != nil {
		return fmt.Sprintf("Kept namespace %s, which this tool created: this tool cannot tell whether anything else is in it (%v). Remove again to retry.", ns, err), nil
	}
	if len(left) > 0 {
		return fmt.Sprintf("Kept namespace %s, which this tool created: it still holds %s. Remove again once they are gone.", ns, strings.Join(left, ", ")), nil
	}
	if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
		return fmt.Sprintf("Kept namespace %s: deleting it failed (%v). Remove again to retry.", ns, err), nil
	}
	m.logs = append(m.logs, "OK: Namespace "+ns+" deleted")
	return "", forget()
}
