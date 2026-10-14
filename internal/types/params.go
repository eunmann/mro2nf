package types

import (
	"errors"
	"fmt"

	"github.com/martian-lang/martian/martian/syntax"
)

// Param roles select which of a callable's parameter sets a bundle's values
// are walked with.
const (
	RoleIn      = "in"      // a callable's input params (mre stage's args, plan)
	RoleOut     = "out"     // a callable's output params (mre stage's job and post, outs)
	RoleMainOut = "mainout" // a split stage's per-chunk main outputs, out + chunkOut (mre stage's job)
	RoleChunkIn = "chunkin" // a split stage's per-chunk input params (mre stage's job)
)

// ErrUnknownRole reports a role outside the Role* constants.
var ErrUnknownRole = errors.New("unknown param role")

// Params returns the callable c's parameter set of role as the members of the
// object the walk descends, as Martian makes a struct of a callable's outputs
// (syntax/struct_type.go structFromCallable). A pipeline has no chunk
// parameters.
func Params(c syntax.Callable, role string) ([]*syntax.StructMember, error) {
	stage, _ := c.(*syntax.Stage)

	switch role {
	case RoleIn:
		return inMembers(c.GetInParams()), nil
	case RoleOut:
		return outMembers(c.GetOutParams()), nil
	case RoleChunkIn:
		if stage == nil {
			return nil, nil
		}

		return inMembers(stage.ChunkIns), nil
	case RoleMainOut:
		out := outMembers(c.GetOutParams())
		if stage != nil {
			out = append(out, outMembers(stage.ChunkOuts)...)
		}

		return out, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
}

func inMembers(ps *syntax.InParams) []*syntax.StructMember {
	if ps == nil {
		return nil
	}

	out := make([]*syntax.StructMember, len(ps.List))
	for i, p := range ps.List {
		out[i] = &syntax.StructMember{Id: p.Id, Tname: p.Tname, Help: p.Help}
	}

	return out
}

func outMembers(ps *syntax.OutParams) []*syntax.StructMember {
	if ps == nil {
		return nil
	}

	out := make([]*syntax.StructMember, len(ps.List))
	for i, p := range ps.List {
		out[i] = &p.StructMember
	}

	return out
}
