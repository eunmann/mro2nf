package emit

// containerRole identifies which image a generated process needs. The transpiler
// owns this mapping (#226): a task's role is a function of its kind — and, for a
// stage task, of the stage's declared adapter — never of what a stage *does*, so
// the assignment is pipeline-agnostic (no CellRanger/tool-specific logic). Only
// the transpiler can compute it correctly, so a user never hand-edits generated
// Nextflow to assign containers.
type containerRole string

const (
	// roleStage runs Martian stage code through the adapter/toolkit, so it needs
	// the user-supplied stage image (today's default). Stage-phase processes carry
	// this role implicitly via the default process.container — they are not
	// label-marked, because the default already points at their image and marking
	// them would only add noise. A future increment can split roleStage per adapter
	// language (roleStageComp/...) by labelling those processes and adding one more
	// withLabel selector; the mechanism does not hardcode a fixed role count.
	roleStage containerRole = "stage"
	// roleDataplane runs only the Go mre binary (bind / forkbind / merge /
	// publish-layout / entryargs): pure orchestration whose content is entirely
	// mro2nf's. It never touches the stage toolkit, so on a fresh-pull backend it
	// can run on a slim base image instead of the heavy stage image.
	roleDataplane containerRole = "dataplane"
)

// label is the Nextflow process label a role maps to. A withLabel: selector in
// nextflow.config keyed on this string points every process of the role at its
// own container image.
func (r containerRole) label() string { return "role_" + string(r) }

// dataplaneLabelLine is the process-body directive that marks a pure-mre task as
// the dataplane role. Every data-plane generator injects it right after the
// `process NAME {` header (two-space body indent, matching the other directives).
// Stage processes are the default role and carry no role marker.
const dataplaneLabelLine = "  label 'role_dataplane'\n"
