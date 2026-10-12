package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/types"
	"github.com/martian-lang/martian/martian/syntax"
)

// errNotEnabled reports an instance its args resolve disabled, which the
// driver ran because its gates held it enabled. The mrp call-graph replay
// (TestMrpCallGraph) tells a disabled instance from a failed task by its
// message.
var errNotEnabled = errors.New("not enabled where its gates said enabled")

// stageFlags are mre stage's: the args, job and post steps' flags, sharing
// the args bundle dir (-args) and the outs bundle a job writes and its post
// stamps (-o).
type stageFlags struct {
	args argsFlags
	run  runFlags
	post postFlags
}

// runStage runs one stage task against a single load of the call graph:
// the task's args step, then its phase's job, then its post step, each
// where the phase has one (stageSteps).
func runStage(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	gf := addGraph(fs)

	var s stageFlags

	s.args.register(fs)
	s.run.register(fs)
	s.post.register(fs)

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	g, man, err := gf.load()
	if err != nil {
		return err
	}

	return s.stage(ctx, g, man)
}

func (s *stageFlags) stage(ctx context.Context, g *cg.Graph, man types.Manifest) error {
	steps, err := stageSteps(g, s.run.phase, s.args.node)
	if err != nil {
		return err
	}

	if steps.args {
		inst, err := s.args.write(g, man, s.run.args, s.run.phase == job.PhaseMain)
		if err != nil {
			return err
		}

		if inst.Disabled {
			return fmt.Errorf("args: %s is %w", inst.Node, errNotEnabled)
		}
	}

	if err := s.run.run(ctx, g, man); err != nil {
		return err
	}

	if !steps.post {
		return nil
	}

	return s.post.post(g, man, s.run.args, s.run.out)
}

// taskSteps are the steps a stage task runs around its job.
type taskSteps struct {
	args, post bool
}

// stageSteps are the steps a task of node's phase runs. The task that runs
// the instance's first job builds its args bundle: a split, or a non-split
// stage's main, whose bundle no other task stages, so it links its files.
// The task that runs its last job posts its outs: a join, or a non-split
// stage's main.
func stageSteps(g *cg.Graph, phase, node string) (taskSteps, error) {
	switch phase {
	case job.PhaseSplit:
		return taskSteps{args: true}, nil
	case job.PhaseJoin:
		return taskSteps{post: true}, nil
	case job.PhaseMain:
		st, err := g.Stage(node)
		if err != nil {
			return taskSteps{}, fmt.Errorf("stage: %w", err)
		}

		s, ok := st.Callable().(*syntax.Stage)
		split := ok && s.Split

		return taskSteps{args: !split, post: !split}, nil
	default:
		return taskSteps{}, fmt.Errorf("%w: %q", errPhase, phase)
	}
}
