package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/common-creation/sandboxxing/internal/config"
	"github.com/common-creation/sandboxxing/internal/state"
)

func testRunner(t *testing.T) (*Runner, *state.State) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ImageDir = cfg.DataDir + "/images"
	cfg.StateFile = cfg.DataDir + "/state.json"
	cfg.PasswordFile = cfg.DataDir + "/password"
	cfg.Bridge = "sbx-test"
	cfg.Subnet = "10.100.0.0/16"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSubnet(cfg.Addr())
	return New(cfg, discardLogger(), nil, st, "host.test"), st
}

func TestParseFlags(t *testing.T) {
	opts, err := parseFlags([]string{
		"-l", "--group=tag", "--name", "demo", "--tag=a,b", "--tag", "c", "pattern",
	}, map[string]flagSpec{
		"l":     {kind: flagBool},
		"group": {kind: flagString},
		"name":  {kind: flagString},
		"tag":   {kind: flagString, repeatable: true, commaList: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.bool("l") {
		t.Error("-l was not recognised")
	}
	if opts.str("group") != "tag" {
		t.Errorf("group = %q", opts.str("group"))
	}
	if opts.str("name") != "demo" {
		t.Errorf("name = %q", opts.str("name"))
	}
	got := strings.Join(opts.strs("tag"), ",")
	if got != "a,b,c" {
		t.Errorf("tags = %q, want a,b,c", got)
	}
	if len(opts.positional) != 1 || opts.positional[0] != "pattern" {
		t.Errorf("positional = %v", opts.positional)
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseFlags([]string{"--nope"}, map[string]flagSpec{}); err == nil {
		t.Error("unknown options must be rejected")
	}
	if _, err := parseFlags([]string{"--name"}, map[string]flagSpec{"name": {kind: flagString}}); err == nil {
		t.Error("missing value must be rejected")
	}
}

func TestSplitAndEnv(t *testing.T) {
	args, err := Split(`new --name "my vm" --env A=1`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new", "--name", "my vm", "--env", "A=1"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("Split = %v, want %v", args, want)
	}
	if _, err := Split(`new "unterminated`); err == nil {
		t.Error("unterminated quotes must fail")
	}
	env, err := parseEnv([]string{"A=1", "B=x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if env["B"] != "x=y" {
		t.Errorf("B = %q", env["B"])
	}
	if _, err := parseEnv([]string{"novalue"}); err == nil {
		t.Error("invalid env entries must fail")
	}
}

func TestHelpAndUnknownCommand(t *testing.T) {
	r, _ := testRunner(t)
	var out, errBuf bytes.Buffer
	sess := &IO{In: strings.NewReader(""), Out: &out, Err: &errBuf}
	if code := r.Run(context.Background(), []string{"help"}, sess); code != 0 {
		t.Errorf("help exit code = %d", code)
	}
	if !strings.Contains(out.String(), "sandboxxing - manage") {
		t.Error("help output is incomplete")
	}
	out.Reset()
	if code := r.Run(context.Background(), []string{"bogus"}, sess); code != 0 {
		t.Errorf("unknown command should print help, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Error("unknown command was not reported")
	}
}

func TestSSHConfigOutput(t *testing.T) {
	r, _ := testRunner(t)
	var out bytes.Buffer
	sess := &IO{In: strings.NewReader(""), Out: &out, Err: &out}
	if code := r.Run(context.Background(), []string{"ssh-config"}, sess); code != 0 {
		t.Fatalf("ssh-config exit code = %d", code)
	}
	text := out.String()
	for _, want := range []string{"Host *.sandbox.example.com", "User %n", "host.test"} {
		if !strings.Contains(text, want) {
			t.Errorf("ssh-config output is missing %q:\n%s", want, text)
		}
	}
}

func TestParseCPUAndSize(t *testing.T) {
	r, _ := testRunner(t)
	if n, err := r.parseCPU("4"); err != nil || n != 4 {
		t.Errorf("parseCPU(4) = %d, %v", n, err)
	}
	if _, err := r.parseCPU("0"); err == nil {
		t.Error("cpu 0 must fail")
	}
	if _, err := r.parseCPU("abc"); err == nil {
		t.Error("cpu abc must fail")
	}
	if n, err := r.parseSize("4G", "memory"); err != nil || n != 4<<30 {
		t.Errorf("parseSize(4G) = %d, %v", n, err)
	}
	if _, err := r.parseSize("-1G", "memory"); err == nil {
		t.Error("negative sizes must fail")
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int64]string{
		30:    "30s",
		90:    "1m30s",
		3700:  "1h1m",
		90000: "1d1h",
	}
	for secs, want := range cases {
		if got := humanDuration(durationOf(secs)); got != want {
			t.Errorf("humanDuration(%ds) = %q, want %q", secs, got, want)
		}
	}
}
