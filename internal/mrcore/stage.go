// Copied from Martian core's martian/core/stage.go (martian-lang/martian
// 50ad6c0, MIT: see LICENSE.martian); TestMrcoreMatchesCore holds it there.

package mrcore

import (
	"encoding/json"
	"path"

	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"
)

// For convenience, a string which implements json.Marshaler.
type jsonString string

func (s jsonString) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

// makeOutArg generates the value with which to pre-populate an entry in the
// _outs json file.
//
// If brokenFileArrays is true, then for backwards compatibility it will return
// a file name for arrays of file types.
//
// An empty array is returned for arrays.  An empty map is returned for maps.
// A map with recursively-populated fields is returned for struct types.
// Nil is returned for any other type.
func makeOutArg(field *syntax.StructMember, filesPath string) json.Marshaler {
	if field.Tname.ArrayDim > 0 {
		return marshallerArray{}
	} else if field.Tname.MapDim > 0 {
		return MarshalerMap{}
	} else if field.IsFile() == syntax.KindIsFile {
		if fn := field.GetOutFilename(); fn != "" {
			return jsonString(path.Join(filesPath, fn))
		}
	}
	return nil
}

func makeOutArgs(outParams *syntax.OutParams, filesPath string, nullAll bool) MarshalerMap {
	args := make(MarshalerMap, len(outParams.List))
	for _, param := range outParams.List {
		if nullAll {
			args[param.Id] = nil
		} else {
			args[param.Id] = makeOutArg(&param.StructMember, filesPath)
		}
	}
	return args
}

func (self *Fork) OutParams() *syntax.OutParams {
	return self.node.call.Callable().GetOutParams()
}

func (self *Fork) verifyOutput(outs LazyArgumentMap) (bool, string) {
	outparams := self.OutParams()
	if len(outparams.List) > 0 {
		if err, alarms := outs.ValidateOutputs(self.node.top.types, outparams); err != nil {
			return false, err.Error() + alarms
		} else if alarms != "" {
			switch syntax.GetEnforcementLevel() {
			case syntax.EnforceError:
				return false, alarms
			case syntax.EnforceAlarm:
				return true, alarms
			case syntax.EnforceLog:
				util.PrintInfo("runtime",
					"(outputs)         %s: WARNING: invalid output\n%s",
					self.fqname, alarms)
				fallthrough
			default:
				return true, ""
			}
		}
	}
	return true, ""
}

func (self *Fork) verifyPipelineOutput(outs json.Marshaler, t syntax.Type) (bool, string) {
	switch t := t.(type) {
	case *syntax.TypedMapType:
		if outs, ok := outs.(MarshalerMap); ok {
			for _, v := range outs {
				if ok, msg := self.verifyPipelineOutput(v, t.Elem); !ok {
					return ok, msg
				}
			}
		}
	case *syntax.ArrayType:
		if outs, ok := outs.(marshallerArray); ok {
			for _, v := range outs {
				if ok, msg := self.verifyPipelineOutput(v, t.Elem); !ok {
					return ok, msg
				}
			}
		}
	case *syntax.StructType:
		if outs, ok := outs.(MarshalerMap); ok {
			if len(t.Members) > 0 {
				if err, alarms := outs.ValidatePipelineOutputs(
					self.node.top.types, t); err != nil {
					return false, err.Error() + alarms
				} else if alarms != "" {
					switch syntax.GetEnforcementLevel() {
					case syntax.EnforceError:
						return false, alarms
					case syntax.EnforceAlarm:
						return true, alarms
					case syntax.EnforceLog:
						util.PrintInfo("runtime",
							"(outputs)         %s: WARNING: invalid output\n%s",
							self.fqname, alarms)
						fallthrough
					default:
						return true, ""
					}
				}
			}
		}
	default:
		util.PrintInfo("runtime", "Invalid output type %T for pipeline", t)
	}
	return true, ""
}
