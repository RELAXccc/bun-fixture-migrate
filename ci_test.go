package fixturemigrate

// The GitHub Action and the GitLab template run the command from a few lines
// of shell. Those lines are run here, as the runner runs them, with a stand-in
// for the command that writes down the arguments it was given.

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// standIn writes a program that records its arguments, one per line, in the
// file $ARGS_OUT names, and exits 3.
func standIn(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$ARGS_OUT\"\n" +
		"printf '%s\\n' \"${BUN_FIXTURE_MIGRATE_CONFIG:-}\" > \"$ARGS_OUT.env\"\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "bun-fixture-migrate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func argsOf(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

type ciCase struct {
	command, args, config string
	want                  []string
}

// Without the config input, -config is not passed, so the job's
// BUN_FIXTURE_MIGRATE_CONFIG is the command's default; a config with a space
// stays one argument; and nothing in args is interpreted.
var ciCases = []ciCase{
	{"status", "-offline -json", "", []string{"status", "-offline", "-json"}},
	{"status", "-offline", "back end/fixture-migrate.yml", []string{"status", "-config", "back end/fixture-migrate.yml", "-offline"}},
	{"check", "", "", []string{"check"}},
	{"plan", "-strict $(touch pwned) `touch pwned`", "", []string{"plan", "-strict", "$(touch", "pwned)", "`touch", "pwned`"}},
	{"version", "", "x.yml", []string{"version"}},
}

func TestTheActionRunsTheCommandItIsGiven(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	data, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Inputs map[string]struct {
			Default string `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Steps []struct {
				ID  string            `yaml:"id"`
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if d := action.Inputs["config"].Default; d != "" {
		t.Fatalf("the config input defaults to %q, which hides BUN_FIXTURE_MIGRATE_CONFIG", d)
	}
	script := ""
	for _, step := range action.Runs.Steps {
		if step.ID == "run" {
			script = step.Run
			if step.Env["BFM_CONFIG"] != "${{ inputs.config }}" || step.Env["BFM_ARGS"] != "${{ inputs.args }}" {
				t.Fatalf("the inputs reach the script through the environment: %v", step.Env)
			}
		}
	}
	if script == "" {
		t.Fatal("no step with id run")
	}
	for _, c := range ciCases {
		tmp := t.TempDir()
		standIn(t, filepath.Join(tmp, "bun-fixture-migrate-bin"))
		out := filepath.Join(tmp, "args")
		output := filepath.Join(tmp, "github_output")
		cmd := exec.Command(bash, "-e", "-c", script)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "RUNNER_TEMP="+tmp, "GITHUB_OUTPUT="+output, "ARGS_OUT="+out,
			"BFM_COMMAND="+c.command, "BFM_ARGS="+c.args, "BFM_CONFIG="+c.config,
			"BUN_FIXTURE_MIGRATE_CONFIG=from/the/job.yml")
		got, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
			t.Fatalf("%+v: the step exits with the command's code, 3: %v\n%s", c, err, got)
		}
		if args := argsOf(t, out); !reflect.DeepEqual(args, c.want) {
			t.Errorf("%+v: ran %q", c, args)
		}
		if env := argsOf(t, out+".env"); env[0] != "from/the/job.yml" {
			t.Errorf("%+v: BUN_FIXTURE_MIGRATE_CONFIG is %q", c, env)
		}
		if data, _ := os.ReadFile(output); string(data) != "exit-code=3\n" {
			t.Errorf("%+v: the output is %q", c, data)
		}
		if _, err := os.Stat(filepath.Join(tmp, "pwned")); err == nil {
			t.Fatalf("%+v: args were run as shell", c)
		}
	}
}

func TestTheGitLabTemplateRunsTheCommandItIsGiven(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	data, err := os.ReadFile(filepath.Join("ci", "gitlab", "bun-fixture-migrate.gitlab-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var jobs map[string]struct {
		Script []string `yaml:"script"`
	}
	if err := yaml.Unmarshal(data, &jobs); err != nil {
		t.Fatal(err)
	}
	script := strings.Join(jobs[".bun-fixture-migrate"].Script, "\n")
	if script == "" {
		t.Fatal("no script in .bun-fixture-migrate")
	}
	for _, c := range ciCases {
		if c.command == "version" {
			continue
		}
		tmp := t.TempDir()
		bin := filepath.Join(tmp, "bin")
		standIn(t, bin)
		work := filepath.Join(tmp, "back end")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(tmp, "args")
		cmd := exec.Command(sh, "-c", script)
		cmd.Dir = tmp
		env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "ARGS_OUT=" + out,
			"BFM_COMMAND=" + c.command, "BFM_ARGS=" + c.args, "BFM_DIR=back end",
			"BUN_FIXTURE_MIGRATE_CONFIG=from/the/job.yml"}
		// GitLab leaves a variable nobody set unset.
		if c.config != "" {
			env = append(env, "BFM_CONFIG="+c.config)
		}
		cmd.Env = env
		got, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
			t.Fatalf("%+v: the job fails with the command's code, 3: %v\n%s", c, err, got)
		}
		if args := argsOf(t, out); !reflect.DeepEqual(args, c.want) {
			t.Errorf("%+v: ran %q", c, args)
		}
		if env := argsOf(t, out+".env"); env[0] != "from/the/job.yml" {
			t.Errorf("%+v: BUN_FIXTURE_MIGRATE_CONFIG is %q", c, env)
		}
		if _, err := os.Stat(filepath.Join(work, "pwned")); err == nil {
			t.Fatalf("%+v: args were run as shell", c)
		}
	}
}
