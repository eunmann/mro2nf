package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// yamlNode is one node of the YAML subset the workflow files use: block
// mappings and sequences, plain and quoted scalars, flow sequences of scalars,
// and block scalars. A block scalar keeps its lines unindented and unfolded,
// minus the blank and `#` lines the line scan drops. A construct outside the
// subset is an error, so the workflow tests fail loudly rather than miss a
// job. go.mod has no YAML module, and these tests don't justify adding one.
type yamlNode struct {
	scalar string
	keys   []string
	fields map[string]*yamlNode
	items  []*yamlNode
}

func (n *yamlNode) get(key string) *yamlNode {
	if n == nil {
		return nil
	}

	return n.fields[key]
}

// list returns a scalar, or a sequence of scalars, as strings.
func (n *yamlNode) list() []string {
	switch {
	case n == nil:
		return nil
	case n.items != nil:
		out := make([]string, 0, len(n.items))
		for _, it := range n.items {
			out = append(out, it.scalar)
		}

		return out
	default:
		return []string{n.scalar}
	}
}

type yamlLine struct {
	num    int
	indent int
	text   string
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

var errYAML = errors.New("unsupported YAML")

func parseYAML(src string) (*yamlNode, error) {
	p := &yamlParser{}

	for i, raw := range strings.Split(src, "\n") {
		raw = strings.TrimRight(raw, " \r")
		text := strings.TrimLeft(raw, " ")
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		p.lines = append(p.lines, yamlLine{num: i + 1, indent: len(raw) - len(text), text: text})
	}

	if len(p.lines) == 0 {
		return &yamlNode{}, nil
	}

	n, err := p.block(p.lines[0].indent)
	if err != nil {
		return nil, err
	}

	if p.pos < len(p.lines) {
		return nil, p.errorf("unexpected indentation")
	}

	return n, nil
}

func (p *yamlParser) errorf(format string, args ...any) error {
	ln := p.lines[min(p.pos, len(p.lines)-1)]

	return fmt.Errorf("%w: line %d %q: %s", errYAML, ln.num, ln.text, fmt.Sprintf(format, args...))
}

func isSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *yamlParser) block(indent int) (*yamlNode, error) {
	if isSeqItem(p.lines[p.pos].text) {
		return p.sequence(indent)
	}

	return p.mapping(indent)
}

func (p *yamlParser) mapping(indent int) (*yamlNode, error) {
	n := &yamlNode{fields: map[string]*yamlNode{}}

	for p.pos < len(p.lines) && p.lines[p.pos].indent >= indent {
		ln := p.lines[p.pos]
		if ln.indent > indent || isSeqItem(ln.text) {
			return nil, p.errorf("expected a mapping key at indent %d", indent)
		}

		key, rest, ok := splitKey(ln.text)
		if !ok {
			return nil, p.errorf("not a mapping entry")
		}

		if _, dup := n.fields[key]; dup {
			return nil, p.errorf("duplicate key %q", key)
		}

		p.pos++

		val, err := p.value(indent, rest)
		if err != nil {
			return nil, err
		}

		n.keys = append(n.keys, key)
		n.fields[key] = val
	}

	return n, nil
}

func (p *yamlParser) sequence(indent int) (*yamlNode, error) {
	n := &yamlNode{items: []*yamlNode{}}

	for p.pos < len(p.lines) && p.lines[p.pos].indent >= indent {
		ln := p.lines[p.pos]
		if ln.indent > indent || !isSeqItem(ln.text) {
			return nil, p.errorf("expected a sequence item at indent %d", indent)
		}

		after := ln.text[1:]
		rest := strings.TrimLeft(after, " ")

		var (
			item *yamlNode
			err  error
		)

		if _, _, isMap := splitKey(rest); isMap && !strings.ContainsAny(rest[:1], `"'[{`) {
			// "- key: v" starts a mapping whose keys sit at the column of "key".
			col := indent + 1 + len(after) - len(rest)
			p.lines[p.pos] = yamlLine{num: ln.num, indent: col, text: rest}
			item, err = p.mapping(col)
		} else {
			p.pos++
			item, err = p.value(indent, rest)
		}

		if err != nil {
			return nil, err
		}

		n.items = append(n.items, item)
	}

	return n, nil
}

// value parses what follows "key:" or "- " on a line at indent.
func (p *yamlParser) value(indent int, rest string) (*yamlNode, error) {
	if strings.HasPrefix(rest, "#") {
		rest = ""
	}

	switch {
	case rest == "":
		if p.pos < len(p.lines) {
			next := p.lines[p.pos]
			if next.indent > indent || (next.indent == indent && isSeqItem(next.text)) {
				return p.block(next.indent)
			}
		}

		return &yamlNode{}, nil
	case rest[0] == '|' || rest[0] == '>':
		var body []string
		for p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			body = append(body, p.lines[p.pos].text)
			p.pos++
		}

		return &yamlNode{scalar: strings.Join(body, "\n")}, nil
	case rest[0] == '{':
		return &yamlNode{scalar: rest}, nil
	case rest[0] == '[':
		return p.flowSequence(rest)
	}

	s, err := p.scalar(rest)

	return &yamlNode{scalar: s}, err
}

func (p *yamlParser) flowSequence(rest string) (*yamlNode, error) {
	end := strings.LastIndexByte(rest, ']')
	if tail := strings.TrimSpace(rest[end+1:]); end < 0 || (tail != "" && !strings.HasPrefix(tail, "#")) {
		return nil, p.errorf("malformed flow sequence")
	}

	n := &yamlNode{items: []*yamlNode{}}

	for elem := range strings.SplitSeq(rest[1:end], ",") {
		if elem = strings.TrimSpace(elem); elem == "" {
			continue
		}

		s, err := p.scalar(elem)
		if err != nil {
			return nil, err
		}

		n.items = append(n.items, &yamlNode{scalar: s})
	}

	return n, nil
}

// scalar returns a plain or quoted scalar without its trailing comment. Quoted
// escapes are not interpreted; the workflows use none.
func (p *yamlParser) scalar(s string) (string, error) {
	if q := s[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(s[1:], q)
		if end < 0 {
			return "", p.errorf("unterminated quoted scalar")
		}

		if tail := strings.TrimSpace(s[end+2:]); tail != "" && !strings.HasPrefix(tail, "#") {
			return "", p.errorf("text after quoted scalar")
		}

		return s[1 : end+1], nil
	}

	if strings.ContainsAny(s[:1], "!&*") {
		return "", p.errorf("tags, anchors and aliases are outside the subset")
	}

	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}

	return strings.TrimSpace(s), nil
}

// splitKey splits "key: rest" or "key:" into key and rest; the bool is false
// for a non-mapping line.
func splitKey(text string) (string, string, bool) {
	i := strings.Index(text, ": ")
	if i < 0 {
		if !strings.HasSuffix(text, ":") {
			return "", "", false
		}

		i = len(text) - 1
	}

	key := strings.Trim(text[:i], `"'`)

	return key, strings.TrimSpace(text[i+1:]), key != ""
}

// TestParseYAMLSubset pins the parser on the constructs the workflow tests
// depend on: a block scalar's "key: value" lines are not keys, "- key:" items
// are mappings, and needs/if values come back as written.
func TestParseYAMLSubset(t *testing.T) {
	src := `on:
  pull_request:
jobs:
  a:
    name: "Job A" # trailing comment
    needs: [x, 'y']
    if: ${{ !cancelled() }}
    steps:
      - name: step
        run: |
          name: not a key
          needs: nope
      - uses: foo@v1
  b:
    needs: a
    strategy:
      matrix:
        include:
          - { k: v }
`

	doc, err := parseYAML(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	jobs := doc.get("jobs")
	if diff := cmp.Diff([]string{"a", "b"}, jobs.keys); diff != "" {
		t.Errorf("job ids (-want +got):\n%s", diff)
	}

	a := jobs.get("a")
	if got := a.get("name").scalar; got != "Job A" {
		t.Errorf("name = %q, want %q", got, "Job A")
	}

	if diff := cmp.Diff([]string{"x", "y"}, a.get("needs").list()); diff != "" {
		t.Errorf("a.needs (-want +got):\n%s", diff)
	}

	if got := a.get("if").scalar; got != "${{ !cancelled() }}" {
		t.Errorf("if = %q", got)
	}

	steps := a.get("steps").items
	if len(steps) != 2 || steps[1].get("uses").scalar != "foo@v1" {
		t.Fatalf("steps parsed wrong: %d items", len(steps))
	}

	if got, want := steps[0].get("run").scalar, "name: not a key\nneeds: nope"; got != want {
		t.Errorf("block scalar = %q, want %q", got, want)
	}

	if diff := cmp.Diff([]string{"a"}, jobs.get("b").get("needs").list()); diff != "" {
		t.Errorf("b.needs (-want +got):\n%s", diff)
	}

	if pr := doc.get("on").get("pull_request"); pr == nil || pr.fields != nil {
		t.Errorf("pull_request should be an empty scalar, got %+v", pr)
	}
}

// TestParseYAMLSubsetRejects checks constructs outside the subset error
// instead of parsing to something the workflow tests would misread.
func TestParseYAMLSubsetRejects(t *testing.T) {
	cases := map[string]string{
		"tag":           "if: !cancelled()\n",
		"duplicate_key": "a: 1\na: 2\n",
		"bad_indent":    "a:\n    b: 1\n  c: 2\n",
		"unterminated":  "a: \"x\n",
		"not_a_mapping": "a: 1\nplain\n",
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseYAML(src); !errors.Is(err, errYAML) {
				t.Errorf("parseYAML(%q) err = %v, want errYAML", src, err)
			}
		})
	}
}
