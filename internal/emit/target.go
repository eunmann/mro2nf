package emit

import "github.com/eunmann/mro2nf/internal/apperror"

// Target is the execution backend the generated project is shaped for. It drives
// the nextflow.config profile, the publish directory, whether a container image
// is required, and whether cloud packaging artifacts (Dockerfile, HealthOmics
// parameter template) are emitted.
type Target string

const (
	// TargetLocal targets a POSIX filesystem: the local executor and the HPC grid
	// profiles (slurm/sge/lsf/pbs/k8s). The default.
	TargetLocal Target = "local"
	// TargetAWSBatch targets AWS Batch with an S3 work dir (classic aws-CLI
	// staging): every process needs a container, and a Dockerfile is emitted.
	TargetAWSBatch Target = "awsbatch"
	// TargetHealthOmics targets AWS HealthOmics private workflows: ECR-only
	// containers, no internet, a managed shared filesystem, outputs published to
	// /mnt/workflow/pubdir, and a zip + parameter-template.json package.
	TargetHealthOmics Target = "healthomics"
)

// ParseTarget validates a -target value.
func ParseTarget(s string) (Target, error) {
	switch Target(s) {
	case TargetLocal, TargetAWSBatch, TargetHealthOmics:
		return Target(s), nil
	case "":
		return TargetLocal, nil
	default:
		return "", &apperror.UnsupportedError{
			Construct: "target " + s,
			Detail:    "expected one of: local, awsbatch, healthomics",
		}
	}
}

// isContainer reports whether the target runs every task in a container image
// (so a default process.container is required and a Dockerfile is emitted).
func (t Target) isContainer() bool {
	return t == TargetAWSBatch || t == TargetHealthOmics
}

// publishDir is the default publish directory for the target. HealthOmics only
// exports files written to its magic /mnt/workflow/pubdir path.
func (t Target) publishDir() string {
	if t == TargetHealthOmics {
		return "/mnt/workflow/pubdir"
	}

	return "results"
}
