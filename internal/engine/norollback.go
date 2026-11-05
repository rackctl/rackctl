package engine

// NoRollbackError marks a phase failure that must NOT trigger a rollback.
//
// The engine tears the platform down when a phase fails, because a phase that fails
// has usually left half-built cloud resources behind. That is right for a failure to
// PROVISION. It is catastrophically wrong for a failure to CONVERGE.
//
// The distinction: provisioning is rackctl's job — VPC, cluster, IAM, the GitOps
// bootstrap. Convergence is the cluster's job — ArgoCD reconciling the catalog until
// every workload is Healthy. If the infrastructure came up correctly and a workload is
// still crashlooping, the infrastructure is not what is broken, and destroying it
// removes the only surface on which the problem can be diagnosed.
//
// The catalog contains workloads that cannot converge until something outside the wait
// completes: opencost crashloops until its metrics reach AMP, which takes minutes of
// scraping. A convergence deadline can therefore expire over a correctly provisioned
// cloud with one workload still settling — and a rollback there destroys the cluster,
// the evidence, and the whole provisioning run to remedy nothing.
//
// A phase returning this says: the cloud is provisioned, something on it has not
// settled, leave it standing and let `rackctl check` say what.
//
// BeforeSpend says more: the phase refused before anything was provisioned. The run touched
// nothing, so the outcome is a refusal rather than a platform left up, whether or not a
// platform was standing when the run began and whether or not the run could tell. Refuse
// builds one.
type NoRollbackError struct {
	Err         error
	BeforeSpend bool
}

func (e *NoRollbackError) Error() string { return e.Err.Error() }
func (e *NoRollbackError) Unwrap() error { return e.Err }
