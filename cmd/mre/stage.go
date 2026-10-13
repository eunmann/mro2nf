package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/eunmann/mro2nf/internal/cg"
	"github.com/eunmann/mro2nf/internal/job"
	"github.com/eunmann/mro2nf/internal/shim"
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
// the args step, then its phase's job, then its post step where the phase
// has one (posts).
func runStage(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	gf := addGraph(fs)

	var s stageFlags

	s.args.register(fs)
	s.run.register(fs)
	s.post.register(fs)

	root := fs.String("s3-mount", "", "the root every task mounts the input buckets under, read-only, whose paths the task's bundles name as they are")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	mount, err := shim.ParseMount(*root)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	roots := shim.NewRoots(string(mount))
	s.args.roots, s.run.roots, s.post.roots = roots, roots, roots

	g, err := gf.load()
	if err != nil {
		return err
	}

	return s.stage(ctx, g)
}

func (s *stageFlags) stage(ctx context.Context, g *cg.Graph) error {
	post, err := posts(g, s.run.phase, s.args.node)
	if err != nil {
		return err
	}

	inst, err := s.args.write(g, s.run.args)
	if err != nil {
		return err
	}

	if inst.Disabled {
		return fmt.Errorf("args: %s is %w", inst.Node, errNotEnabled)
	}

	if err := s.run.run(ctx, g); err != nil {
		return err
	}

	if !post {
		return nil
	}

	return s.post.post(g, s.run.args, s.run.out)
}

// posts reports whether a task of node's phase runs the instance's last
// job, so posts its outs: a join, or a non-split stage's main.
func posts(g *cg.Graph, phase, node string) (bool, error) {
	switch phase {
	case job.PhaseSplit:
		return false, nil
	case job.PhaseJoin:
		return true, nil
	case job.PhaseMain:
		st, err := g.Stage(node)
		if err != nil {
			return false, fmt.Errorf("stage: %w", err)
		}

		s, ok := st.Callable().(*syntax.Stage)

		return !ok || !s.Split, nil
	default:
		return false, fmt.Errorf("%w: %q", errPhase, phase)
	}
}
