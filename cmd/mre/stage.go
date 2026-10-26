package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"

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

// stageFlags are mre stage's: the args, job and post steps' flags. The
// steps hand each other the instance, its args and its outs in memory.
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

	root := fs.String("s3-mount", "", "the root every task mounts the input buckets under, read-only, whose paths the task's bundles name as they are, as they do the entry file values every task reads in place")
	host := fs.Bool("host-in-place", false, "every task reads a head-node entry file value in place, as PLAN planned it under the same flag")
	runWork := fs.String("run-workdir", "", "the run's work dir, whose tasks' files are under no read-only root; a URI holds no path")

	if err := fs.Parse(argv); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	mount, err := shim.ParseMount(*root)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	g, err := gf.load()
	if err != nil {
		return err
	}

	roots, err := taskRoots(g, cg.ReadInPlace{Mount: mount, Host: *host}, s.run.rt.Work, *runWork)
	if err != nil {
		return err
	}

	s.args.roots, s.run.roots = roots, roots

	return s.stage(ctx, g)
}

// taskRoots are the run's read-only roots as a task in work reads them:
// none holds the task's own files, nor, where the run's work dir runWork is
// a path, any task's, which an include path may hold.
func taskRoots(g *cg.Graph, in cg.ReadInPlace, work, runWork string) (shim.Roots, error) {
	roots, err := g.Roots(in, codeDirs(g)...)
	if err != nil {
		return shim.Roots{}, fmt.Errorf("stage: %w", err)
	}

	except := []string{work}
	if filepath.IsAbs(runWork) {
		except = append(except, runWork)
	}

	for _, dir := range except {
		if roots, err = roots.Except(dir); err != nil {
			return shim.Roots{}, fmt.Errorf("stage: %w", err)
		}
	}

	return roots, nil
}

// codeDirs are the dirs of g's stages' code, each found as mrp finds it
// (job.NewStage): a py stage's own dir, and the dir of any other stage's
// program. A stage whose code is not found adds none, as nothing lies in
// it; the task that runs it fails finding it.
func codeDirs(g *cg.Graph) []string {
	var dirs []string

	for st := range g.Stages() {
		s, err := job.NewStage(st, g.MroPaths())
		if err != nil {
			continue
		}

		if s.Code.Type == syntax.PythonStage {
			dirs = append(dirs, s.Code.Cmd)
		} else {
			dirs = append(dirs, filepath.Dir(s.Code.Cmd))
		}
	}

	return dirs
}

func (s *stageFlags) stage(ctx context.Context, g *cg.Graph) error {
	post, err := posts(g, s.run.phase, s.args.node)
	if err != nil {
		return err
	}

	if s.run.staged, err = shim.NewStaged(append(slices.Clone(s.args.in), s.args.refs, s.run.chunk, s.run.chunks, s.run.chunkOuts)...); err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	if s.run.refs, err = s.run.staged.Leaves(); err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	inst, args, err := s.args.write(g, s.run.args, s.run.refs)
	if err != nil {
		return err
	}

	if inst.Disabled {
		return fmt.Errorf("args: %s is %w", inst.Node, errNotEnabled)
	}

	done, err := s.run.run(ctx, g, inst, args)
	if err != nil || !post {
		return err
	}

	return s.post.post(g, inst, done, s.run.staged)
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
