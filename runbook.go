package runn

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Songmu/axslogparser"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
	"github.com/k1LoW/curlreq"
	"github.com/k1LoW/expand"
	"github.com/k1LoW/grpcurlreq"
	"gopkg.in/yaml.v2"
)

type position struct {
	Line int
	// Column int
}

type area struct {
	Start *position
	End   *position
}

type areas struct {
	Desc    *area
	Runners *area
	Vars    *area
	Steps   []*area
}

type runbook struct {
	Desc        string          `yaml:"desc"`
	Runners     map[string]any  `yaml:"runners,omitempty"`
	Vars        map[string]any  `yaml:"vars,omitempty"`
	Steps       []yaml.MapSlice `yaml:"steps"`
	Debug       bool            `yaml:"debug,omitempty"`
	Interval    string          `yaml:"interval,omitempty"`
	If          string          `yaml:"if,omitempty"`
	SkipTest    bool            `yaml:"skipTest,omitempty"`
	Loop        any             `yaml:"loop,omitempty"`
	Concurrency string          `yaml:"concurrency,omitempty"`
	Force       bool            `yaml:"force,omitempty"`

	useMap   bool
	stepKeys []string
}

type runbookMapped struct {
	Desc        string         `yaml:"desc,omitempty"`
	Runners     map[string]any `yaml:"runners,omitempty"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	Steps       yaml.MapSlice  `yaml:"steps,omitempty"`
	Debug       bool           `yaml:"debug,omitempty"`
	Interval    string         `yaml:"interval,omitempty"`
	If          string         `yaml:"if,omitempty"`
	SkipTest    bool           `yaml:"skipTest,omitempty"`
	Loop        any            `yaml:"loop,omitempty"`
	Concurrency string         `yaml:"concurrency,omitempty"`
	Force       bool           `yaml:"force,omitempty"`
}

func NewRunbook(desc string) *runbook {
	const defaultDesc = "Generated by `runn new`"
	if desc == "" {
		desc = defaultDesc
	}
	r := &runbook{
		Desc:    desc,
		Runners: map[string]any{},
		Vars:    map[string]any{},
		Steps:   []yaml.MapSlice{},
	}
	return r
}

func ParseRunbook(in io.Reader) (*runbook, error) {
	b, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	return parseRunbook(b)
}

func parseRunbook(b []byte) (*runbook, error) {
	rb := NewRunbook("")

	repFn := expand.InterpolateRepFn(os.LookupEnv)
	rep, err := expand.ReplaceYAML(string(b), repFn)
	if err != nil {
		return nil, err
	}
	b = []byte(rep)
	if err := yaml.Unmarshal(b, rb); err != nil {
		if err := parseRunbookMapped(b, rb); err != nil {
			return nil, err
		}
	}

	return rb, nil
}

func parseRunbookMapped(b []byte, rb *runbook) error {
	m := &runbookMapped{}
	if err := yaml.Unmarshal(b, m); err != nil {
		return err
	}
	rb.useMap = true
	rb.Desc = m.Desc
	rb.Runners = m.Runners
	rb.Vars = m.Vars
	rb.Debug = m.Debug
	rb.Interval = m.Interval
	rb.If = m.If
	rb.SkipTest = m.SkipTest
	rb.Force = m.Force

	keys := map[string]struct{}{}
	for _, s := range m.Steps {
		k, ok := s.Key.(string)
		if !ok {
			return fmt.Errorf("failed to parse as mapped steps: %v", s)
		}
		v, ok := s.Value.(yaml.MapSlice)
		if !ok {
			return fmt.Errorf("failed to parse as mapped steps: %v", s)
		}
		rb.stepKeys = append(rb.stepKeys, k)
		rb.Steps = append(rb.Steps, v)
		if _, ok := keys[k]; ok {
			return fmt.Errorf("duplicate step keys: %s", k)
		}
		keys[k] = struct{}{}
	}

	return nil
}

func (rb *runbook) AppendStep(in ...string) error {
	if len(in) == 0 {
		return errors.New("no argument")
	}
	if rb.useMap {
		key := fmt.Sprintf("%s%d", in[0], len(rb.stepKeys))
		rb.stepKeys = append(rb.stepKeys, key)
	}
	switch {
	case strings.HasPrefix(in[0], "curl"):
		return rb.curlToStep(in...)
	case strings.HasPrefix(in[0], "grpcurl"):
		return rb.grpcurlToStep(in...)
	default:
		if len(in) == 1 {
			if err := rb.axsLogToStep(in...); err == nil {
				return nil
			}
		}
		return rb.cmdToStep(in...)
	}
}

func (rb *runbook) MarshalYAML() (any, error) {
	if !rb.useMap {
		return rb, nil
	}
	if len(rb.stepKeys) != len(rb.Steps) {
		return nil, errors.New("invalid runbook")
	}
	m := &runbookMapped{}
	m.Desc = rb.Desc
	m.Runners = rb.Runners
	m.Vars = rb.Vars
	m.Debug = rb.Debug
	m.Interval = rb.Interval
	m.If = rb.If
	m.SkipTest = rb.SkipTest
	m.Force = rb.Force
	ms := yaml.MapSlice{}
	for i, k := range rb.stepKeys {
		ms = append(ms, yaml.MapItem{
			Key:   k,
			Value: rb.Steps[i],
		})
	}
	m.Steps = ms
	return m, nil
}

func (rb *runbook) curlToStep(in ...string) error {
	req, err := curlreq.NewRequest(in...)
	if err != nil {
		return err
	}

	splitted := strings.Split(req.URL.String(), req.URL.Host)
	dsn := fmt.Sprintf("%s%s", splitted[0], req.URL.Host)
	key := rb.setRunner(dsn)
	step, err := CreateHTTPStepMapSlice(key, req)
	if err != nil {
		return err
	}
	rb.Steps = append(rb.Steps, step)
	return nil
}

func (rb *runbook) grpcurlToStep(in ...string) error {
	p, err := grpcurlreq.Parse(in...)
	if err != nil {
		return err
	}
	if p.Addr == "" || p.Method == "" || p.SubCmd != "" {
		return fmt.Errorf("unsupported grpcurl command: %v", in)
	}
	dsn := fmt.Sprintf("grpc://%s", p.Addr)
	key := rb.setRunner(dsn)

	hm := yaml.MapSlice{}
	h := map[string]string{}
	for k, v := range p.Headers {
		h[k] = v[0]
	}
	if len(h) > 0 {
		hm = append(hm, yaml.MapItem{
			Key:   "headers",
			Value: h,
		})
	}

	// messages
	switch {
	case len(p.Messages) == 1:
		hm = append(hm, yaml.MapItem{
			Key:   "message",
			Value: p.Messages[0],
		})
	case len(p.Messages) > 1:
		hm = append(hm, yaml.MapItem{
			Key:   "messages",
			Value: p.Messages,
		})
	}

	if len(hm) == 0 {
		hm = nil
	}
	step := yaml.MapSlice{
		{Key: key, Value: yaml.MapSlice{
			{Key: p.Method, Value: hm},
		}},
	}
	rb.Steps = append(rb.Steps, step)
	return nil
}

func (rb *runbook) setRunner(dsn string) string {
	const (
		httpRunnerKeyPrefix = "req"
		grpcRunnerKeyPrefix = "greq"
		dbRunnerKeyPrefix   = "db"
	)
	var hc, gc, dc int
	for k, v := range rb.Runners {
		vv, ok := v.(string)
		if !ok {
			continue
		}
		if vv == dsn {
			return k
		}
		switch {
		case strings.HasPrefix(vv, "http"):
			hc += 1
		case strings.HasPrefix(vv, "grpc"):
			gc += 1
		default:
			dc += 1
		}
	}

	var key string
	switch {
	case strings.HasPrefix(dsn, "http"):
		if hc > 0 {
			key = fmt.Sprintf("%s%d", httpRunnerKeyPrefix, hc+1)
		} else {
			key = httpRunnerKeyPrefix
		}
	case strings.HasPrefix(dsn, "grpc"):
		if gc > 0 {
			key = fmt.Sprintf("%s%d", grpcRunnerKeyPrefix, gc+1)
		} else {
			key = grpcRunnerKeyPrefix
		}
	default:
		if dc > 0 {
			key = fmt.Sprintf("%s%d", dbRunnerKeyPrefix, dc+1)
		} else {
			key = dbRunnerKeyPrefix
		}
	}
	rb.Runners[key] = dsn
	return key
}

func (rb *runbook) axsLogToStep(in ...string) error {
	const dummyDSN = "https://dummy.example.com"
	line := strings.Join(in, " ")
	_, l, err := axslogparser.GuessParser(line)
	if err != nil {
		return err
	}
	dsn := dummyDSN
	key := rb.setRunner(dsn)
	req, err := http.NewRequest(l.Method, l.RequestURI, nil)
	if err != nil {
		return err
	}
	if l.UserAgent != "" {
		req.Header.Add("User-Agent", l.UserAgent)
	}
	step, err := CreateHTTPStepMapSlice(key, req)
	if err != nil {
		return err
	}
	rb.Steps = append(rb.Steps, step)
	return nil
}

func (rb *runbook) cmdToStep(in ...string) error {
	step := yaml.MapSlice{
		{Key: execRunnerKey, Value: yaml.MapSlice{
			{Key: "command", Value: joinCommands(in...)},
		}},
	}
	rb.Steps = append(rb.Steps, step)
	return nil
}

func (rb *runbook) toBook() (*book, error) {
	var (
		ok  bool
		err error
	)
	bk := newBook()
	bk.desc = rb.Desc
	bk.runners, ok = normalize(rb.Runners).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("failed to normalize runners: %v", rb.Runners)
	}
	bk.vars, ok = normalize(rb.Vars).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("failed to normalize vars: %v", rb.Vars)
	}
	for _, s := range rb.Steps {
		v, ok := normalize(s).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("failed to normalize step values: %v", s)
		}
		bk.rawSteps = append(bk.rawSteps, v)
	}
	bk.debug = rb.Debug
	bk.intervalStr = rb.Interval
	bk.ifCond = rb.If
	bk.skipTest = rb.SkipTest
	bk.force = rb.Force
	if rb.Loop != nil {
		bk.loop, err = newLoop(rb.Loop)
		if err != nil {
			return nil, err
		}
	}
	bk.concurrency = rb.Concurrency
	bk.useMap = rb.useMap
	bk.stepKeys = rb.stepKeys

	return bk, nil
}

func joinCommands(in ...string) string {
	var cmd []string
	for _, i := range in {
		i = strings.TrimSuffix(i, "\n")
		if strings.Contains(i, " ") {
			cmd = append(cmd, fmt.Sprintf("%#v", i))
		} else {
			cmd = append(cmd, i)
		}
	}
	return strings.Join(cmd, " ") + "\n"
}

// normalize unmarshaled values.
func normalize(v any) any {
	switch v := v.(type) {
	case []any:
		res := make([]any, len(v))
		for i, vv := range v {
			res[i] = normalize(vv)
		}
		return res
	case map[any]any:
		res := make(map[string]any)
		for k, vv := range v {
			res[fmt.Sprintf("%v", k)] = normalize(vv)
		}
		return res
	case map[string]any:
		res := make(map[string]any)
		for k, vv := range v {
			res[k] = normalize(vv)
		}
		return res
	case []map[string]any:
		res := make([]map[string]any, len(v))
		for i, vv := range v {
			var ok bool
			res[i], ok = normalize(vv).(map[string]any)
			if !ok {
				return fmt.Errorf("failed to normalize: %v", vv)
			}
		}
		return res
	case yaml.MapSlice:
		res := make(map[string]any)
		for _, i := range v {
			res[fmt.Sprintf("%v", i.Key)] = normalize(i.Value)
		}
		return res
	case int:
		if v < 0 {
			return int64(v)
		}
		return uint64(v)
	default:
		return v
	}
}

func detectRunbookAreas(in string) *areas {
	a := &areas{}
	tokens := lexer.Tokenize(in)
	parsed, err := parser.Parse(tokens, 0)
	if err != nil {
		return a
	}
	m, ok := parsed.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return a
	}
	sections := m.Values
	for _, s := range sections {
		key, ok := s.Key.(*ast.StringNode)
		if !ok {
			return a
		}
		switch key.Value {
		case "desc":
			a.Desc = detectAreaFromNode(s)
		case "vars":
			a.Vars = detectAreaFromNode(s)
		case "runners":
			a.Runners = detectAreaFromNode(s)
		case "steps":
			switch steps := s.Value.(type) {
			case *ast.MappingNode:
				for _, v := range steps.Values {
					a.Steps = append(a.Steps, detectAreaFromNode(v))
				}
			case *ast.SequenceNode:
				for _, v := range steps.Values {
					aa := detectAreaFromNode(v)
					// Get `-` token
					t := v.GetToken()
					for {
						if t.Value == "-" {
							aa.Start = &position{
								Line: t.Position.Line,
							}
							break
						}
						t = t.Prev
					}
					a.Steps = append(a.Steps, aa)
				}
			}
		}
	}

	return a
}

type areaDetector struct {
	start *token.Token
	end   *token.Token
}

// Visit implements ast.Visitor interface.
// It detects the start and end token of the area.
func (d *areaDetector) Visit(node ast.Node) ast.Visitor {
	if d.start == nil {
		d.start = node.GetToken()
	}
	if d.end == nil {
		d.end = node.GetToken()
	}
	if d.start.Position.Line > node.GetToken().Position.Line ||
		(d.start.Position.Line == node.GetToken().Position.Line && d.start.Position.Column > node.GetToken().Position.Column) {
		d.start = node.GetToken()
	}
	if d.end.Position.Line < node.GetToken().Position.Line ||
		(d.end.Position.Line == node.GetToken().Position.Line && d.end.Position.Column < node.GetToken().Position.Column) {
		d.end = node.GetToken()
	}
	return d
}

// detectAreaFromNode detects the start and end position of the area from the node.
func detectAreaFromNode(node ast.Node) *area {
	d := &areaDetector{}
	ast.Walk(d, node)
	a := &area{
		Start: &position{
			Line: d.start.Position.Line,
		},
		End: &position{
			Line: d.end.Position.Line,
		},
	}
	if (strings.Count(d.end.Value, "\n") - 1) > 0 {
		if d.end.Next != nil {
			a.End.Line += strings.Count(d.end.Value, "\n") - 1
		}
	}
	return a
}

func pickStepYAML(in string, idx int) (string, error) {
	repFn := expand.InterpolateRepFn(os.LookupEnv)
	rep, err := expand.ReplaceYAML(in, repFn)
	if err != nil {
		return "", err
	}
	a := detectRunbookAreas(rep)
	if len(a.Steps)-1 < idx {
		return "", fmt.Errorf("step not found: %d", idx)
	}
	step := a.Steps[idx]
	start := step.Start.Line
	end := step.End.Line
	lines := strings.Split(rep, "\n")
	if len(lines) < end {
		return "", fmt.Errorf("line not found: %d", end)
	}
	w := len(strconv.Itoa(end))
	var picked []string
	for i := start; i <= end; i++ {
		picked = append(picked, yellow(fmt.Sprintf("%s ", fmt.Sprintf(fmt.Sprintf("%%%dd", w), i)))+lines[i-1])
	}
	return strings.Join(picked, "\n"), nil
}
