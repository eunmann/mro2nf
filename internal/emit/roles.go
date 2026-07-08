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
	// no role_stage marker: they already carry `label 'lang_<lang>'` (stageDirectives)
	// and the default process.container points at their image, so a second marker
	// would only add noise. A future increment can split roleStage per adapter
	// language (roleStageComp/...) by keying withLabel selectors on those existing
	// lang_ labels; the mechanism does not hardcode a fixed role count.
	roleStage containerRole = "stage"
	// roleDataplane runs only the Go mre binary (bind / forkbind / merge /
	// publish-layout / entryargs): pure orchestration whose content is entirely
	// mro2nf's. It never touches the stage toolkit, so on a fresh-pull backend it
	// can run on a slim base image instead of the heavy stage image.
	roleDataplane containerRole = "dataplane"
)

// rolePrefix namespaces role labels apart from the stage `lang_` labels, so the
// two label families never collide in a withLabel: selector.
const rolePrefix = "role_"

// label is the Nextflow process label a role maps to. A withLabel: selector in
// nextflow.config keyed on this string points every process of the role at its
// own container image.
func (r containerRole) label() string { return rolePrefix + string(r) }

// dataplaneLabelLine is the process-body directive that marks a pure-mre task as
// the dataplane role. Every data-plane generator injects it right after the
// `process NAME {` header (two-space body indent, matching the other directives).
// Stage processes are the default role and carry no role marker. Built from the
// same rolePrefix + roleDataplane the config withLabel: selector (emit.go, via
// roleDataplane.label()) uses, so the marker and the selector stay a single source
// of truth. A const expression (not a method call) keeps it a compile-time constant.
const dataplaneLabelLine = "  label '" + rolePrefix + string(roleDataplane) + "'\n"
