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
// Observed: a fresh install provisioned cleanly, ArgoCD generated all 44 Applications,
// 42 of them went Healthy, and opencost was still crashlooping (it fails until metrics
// reach AMP, which takes minutes). The 30-minute convergence wait expired, the phase
// failed, and the engine destroyed a working EKS cluster — losing the evidence and
// forty minutes of provisioning, because one workload needed five more.
//
// A phase returning this says: the cloud is provisioned, something on it has not
// settled, leave it standing and let `rackctl doctor` say what.
type NoRollbackError struct{ Err error }

func (e *NoRollbackError) Error() string { return e.Err.Error() }
func (e *NoRollbackError) Unwrap() error { return e.Err }
