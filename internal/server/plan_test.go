package server

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nektos/act/pkg/model"
)

const pipeline = `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      targets: ${{ steps.t.outputs.targets }}
    steps: [{id: t, run: echo}]
  test:
    needs: build
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
        go: ['1.25', '1.26']
        exclude:
          - {os: macos-latest, go: '1.25'}
    runs-on: ${{ matrix.os }}
    steps: [{run: echo}]
  package:
    needs: build
    name: package ${{ matrix.target }}
    strategy:
      matrix:
        target: ${{ fromJSON(needs.build.outputs.targets) }}
    runs-on: [self-hosted, "${{ matrix.target }}"]
    steps: [{run: echo}]
  deploy:
    needs: [test, package]
    if: github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps: [{run: echo}]
  notify:
    needs: deploy
    if: always()
    runs-on: ubuntu-latest
    steps: [{run: echo}]
`

// sim keeps a run's rows in memory and applies what progress decides.
type sim struct {
	t    *testing.T
	run  *Run
	rows []*Job
}

func newSim(t *testing.T, src, ref string) *sim {
	s := &sim{t: t, run: &Run{ID: "r1", Repo: "o/app", SHA: "abc", Ref: ref, Event: "push", Name: "ci", Source: []byte(src), Payload: []byte(`{"sender":{"login":"me"}}`)}}
	s.step()
	return s
}

func (s *sim) step() *change {
	s.t.Helper()
	c, err := progress(s.run, s.rows)
	if err != nil {
		s.t.Fatal(err)
	}
	s.rows = append(s.rows, c.added...)
	for _, j := range c.cancel {
		j.Status = Cancelled
	}
	if len(c.cancel) > 0 {
		s.step()
	}
	return c
}

// state lists "name=status" for every row, in creation order.
func (s *sim) state() string {
	var out []string
	for _, j := range s.rows {
		if j.Expand {
			out = append(out, j.Key+"=placeholder")
			continue
		}
		out = append(out, j.Name+"="+j.Status)
	}
	return strings.Join(out, ", ")
}

func (s *sim) job(name string) *Job {
	for _, j := range s.rows {
		if j.Name == name {
			return j
		}
	}
	s.t.Fatalf("no job %q in %s", name, s.state())
	return nil
}

func (s *sim) finish(name, status string, outputs map[string]string) {
	j := s.job(name)
	if j.Status != Queued {
		s.t.Fatalf("%s is %s, not queued: %s", name, j.Status, s.state())
	}
	j.Status, j.Outputs = status, outputs
	s.step()
}

func (s *sim) expect(want string) {
	s.t.Helper()
	if got := s.state(); got != want {
		s.t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func TestProgressRunsJobsInDependencyOrder(t *testing.T) {
	s := newSim(t, pipeline, "refs/heads/main")
	s.expect("build=queued, test (ubuntu-latest, 1.25)=waiting, test (ubuntu-latest, 1.26)=waiting, test (macos-latest, 1.26)=waiting, package=placeholder, deploy=waiting, notify=waiting")
	if got := s.job("build").Labels; !slices.Equal(got, []string{"ubuntu-latest"}) {
		t.Errorf("build labels = %v", got)
	}

	s.finish("build", Success, map[string]string{"targets": `["android","ios"]`})
	s.expect("build=success, test (ubuntu-latest, 1.25)=queued, test (ubuntu-latest, 1.26)=queued, test (macos-latest, 1.26)=queued, package android=queued, deploy=waiting, notify=waiting, package ios=queued")
	if got := s.job("test (macos-latest, 1.26)").Labels; !slices.Equal(got, []string{"macos-latest"}) {
		t.Errorf("runs-on with matrix = %v", got)
	}
	if got := s.job("package ios").Labels; !slices.Equal(got, []string{"self-hosted", "ios"}) {
		t.Errorf("runs-on from a matrix expanded from needs = %v", got)
	}
	if m := s.job("package android").Matrix; fmt.Sprint(m) != "map[target:android]" {
		t.Errorf("matrix = %v", m)
	}

	for _, n := range []string{"test (ubuntu-latest, 1.25)", "test (ubuntu-latest, 1.26)", "test (macos-latest, 1.26)", "package android"} {
		s.finish(n, Success, nil)
	}
	s.expect("build=success, test (ubuntu-latest, 1.25)=success, test (ubuntu-latest, 1.26)=success, test (macos-latest, 1.26)=success, package android=success, deploy=waiting, notify=waiting, package ios=queued")
	s.finish("package ios", Success, nil)
	s.finish("deploy", Success, nil)
	s.finish("notify", Success, nil)

	needs := needsFor(mustWorkflow(t, pipeline), "deploy", s.rows)
	if fmt.Sprint(needs) != "map[build:{success map[targets:[\"android\",\"ios\"]]} package:{success map[]} test:{success map[]}]" {
		t.Errorf("deploy's transitive needs = %v", needs)
	}
}

func TestProgressFailFastSkipsAndAlways(t *testing.T) {
	s := newSim(t, pipeline, "refs/heads/main")
	s.finish("build", Success, map[string]string{"targets": `["android"]`})
	s.job("test (ubuntu-latest, 1.26)").Status = Running

	// One combination fails: fail-fast cancels the others, deploy is skipped
	// (its needs failed), notify runs anyway (if: always()).
	s.finish("test (ubuntu-latest, 1.25)", Failure, nil)
	s.expect("build=success, test (ubuntu-latest, 1.25)=failure, test (ubuntu-latest, 1.26)=cancelled, test (macos-latest, 1.26)=cancelled, package android=queued, deploy=waiting, notify=waiting")
	s.finish("package android", Success, nil)
	s.expect("build=success, test (ubuntu-latest, 1.25)=failure, test (ubuntu-latest, 1.26)=cancelled, test (macos-latest, 1.26)=cancelled, package android=success, deploy=skipped, notify=queued")
}

func TestProgressIfAndMaxParallel(t *testing.T) {
	src := `on: push
jobs:
  lint:
    if: github.actor == 'someone-else'
    runs-on: ubuntu-latest
    steps: [{run: echo}]
  shard:
    strategy:
      max-parallel: 1
      fail-fast: false
      matrix:
        n: [1, 2, 3]
    runs-on: ubuntu-latest
    steps: [{run: echo}]
  bad:
    runs-on: ubuntu-latest
    if: ${{ fromJSON('{') }}
    steps: [{run: echo}]
`
	s := newSim(t, src, "refs/heads/feature")
	s.expect("lint=skipped, shard (1)=queued, shard (2)=waiting, shard (3)=waiting, bad=failure")
	s.finish("shard (1)", Failure, nil)
	s.expect("lint=skipped, shard (1)=failure, shard (2)=queued, shard (3)=waiting, bad=failure")
	s.finish("shard (2)", Success, nil)
	s.expect("lint=skipped, shard (1)=failure, shard (2)=success, shard (3)=queued, bad=failure")

	// The deploy job of the pipeline only runs on main.
	p := newSim(t, pipeline, "refs/heads/feature")
	p.finish("build", Success, map[string]string{"targets": `["x"]`})
	for _, n := range []string{"test (ubuntu-latest, 1.25)", "test (ubuntu-latest, 1.26)", "test (macos-latest, 1.26)", "package x"} {
		p.finish(n, Success, nil)
	}
	if st := p.job("deploy").Status; st != Skipped {
		t.Errorf("deploy on a feature branch is %s, want skipped: %s", st, p.state())
	}
	if st := p.job("notify").Status; st != Queued {
		t.Errorf("notify is %s: %s", st, p.state())
	}

	// A matrix job skipped before it was expanded shows under its own name.
	q := newSim(t, pipeline, "refs/heads/main")
	q.finish("build", Failure, nil)
	if j := q.job("package"); j.Status != Skipped || j.Expand {
		t.Errorf("package = %+v: %s", j, q.state())
	}

	// A matrix that expands to nothing fails, as on GitHub.
	e := newSim(t, pipeline, "refs/heads/main")
	e.finish("build", Success, map[string]string{"targets": `[]`})
	for _, j := range e.rows {
		if j.Key == "package" && j.Status != Failure {
			t.Errorf("package with an empty matrix is %s: %s", j.Status, e.state())
		}
	}
}

func TestValidateNeeds(t *testing.T) {
	cycle := "on: push\njobs:\n  a: {needs: b, runs-on: x, steps: [{run: echo}]}\n  b: {needs: a, runs-on: x, steps: [{run: echo}]}\n"
	if err := validateNeeds(mustWorkflow(t, cycle)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle: %v", err)
	}
	missing := "on: push\njobs:\n  a: {needs: z, runs-on: x, steps: [{run: echo}]}\n"
	if err := validateNeeds(mustWorkflow(t, missing)); err == nil || !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("unknown need: %v", err)
	}
	if err := validateNeeds(mustWorkflow(t, pipeline)); err != nil {
		t.Error(err)
	}
}

func TestToExpression(t *testing.T) {
	cases := map[string]string{
		"github.ref == 'x'":           "github.ref == 'x'",
		"${{ matrix.os }}":            "matrix.os",
		"build ${{ matrix.os }}":      "format('build {0}', matrix.os)",
		"a {b} ${{ 'c}}' }} ${{ d }}": "format('a {{b}} {0} {1}', 'c}}', d)",
		"it's ${{ x }}":               "format('it''s {0}', x)",
	}
	for in, want := range cases {
		if got, err := toExpression(in, false); err != nil || got != want {
			t.Errorf("toExpression(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := toExpression("${{ x", false); err == nil {
		t.Error("unclosed expression accepted")
	}
}

func mustWorkflow(t *testing.T, src string) *model.Workflow {
	t.Helper()
	wf, err := model.ReadWorkflow(strings.NewReader(src), false)
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func TestProgressCancelledCombinationKeepsTheIf(t *testing.T) {
	src := `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: echo}]
  deploy:
    needs: build
    if: github.ref == 'refs/heads/main'
    strategy:
      matrix:
        region: [eu, us]
    runs-on: ubuntu-latest
    steps: [{run: echo}]
`
	s := newSim(t, src, "refs/heads/feature")
	s.job("deploy (eu)").Status = Cancelled // cancelled through the API while it waited
	s.finish("build", Success, nil)
	if st := s.job("deploy (us)").Status; st != Skipped {
		t.Errorf("deploy (us) is %s, want skipped by its if: %s", st, s.state())
	}
}
