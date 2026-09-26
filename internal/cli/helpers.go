package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/common-creation/sandboxxing/internal/config"
)

// flagKind distinguishes value taking options from switches.
type flagKind int

const (
	flagBool flagKind = iota
	flagString
)

type flagSpec struct {
	kind       flagKind
	hasValue   bool // boolean flags that may take an explicit value
	repeatable bool
	commaList  bool
	def        string
}

// options is the result of parseFlags.
type options struct {
	positional []string
	values     map[string][]string
	switches   map[string]bool
}

func (o *options) str(name string) string {
	if v := o.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (o *options) strs(name string) []string { return o.values[name] }

func (o *options) bool(name string) bool {
	if v, ok := o.switches[name]; ok && v {
		return true
	}
	if strings.EqualFold(o.str(name), "true") || o.str(name) == "1" {
		return true
	}
	return false
}

// boolDefault reports the switch value, falling back to def when unset.
func (o *options) boolDefault(name string, def bool) bool {
	if _, ok := o.switches[name]; ok {
		return o.bool(name)
	}
	if _, ok := o.values[name]; ok {
		return o.bool(name)
	}
	return def
}

// parseFlags parses a GNU style command line. Both --name=value and
// --name value are accepted, as are repeated options such as --tag.
func parseFlags(args []string, specs map[string]flagSpec) (*options, error) {
	opts := &options{values: map[string][]string{}, switches: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.positional = append(opts.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			opts.positional = append(opts.positional, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		value := ""
		hasInline := false
		if eq := strings.Index(name, "="); eq >= 0 {
			name, value = name[:eq], name[eq+1:]
			hasInline = true
		}
		spec, known := specs[name]
		if !known {
			return nil, fmt.Errorf("unknown option %s", arg)
		}
		switch spec.kind {
		case flagBool:
			if hasInline {
				opts.values[name] = append(opts.values[name], value)
				opts.switches[name] = value == "" || strings.EqualFold(value, "true") || value == "1"
			} else if spec.hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				opts.values[name] = append(opts.values[name], args[i])
				opts.switches[name] = strings.EqualFold(args[i], "true") || args[i] == "1"
			} else {
				opts.switches[name] = true
			}
		case flagString:
			if !hasInline {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("option %s requires a value", arg)
				}
				i++
				value = args[i]
			}
			if spec.commaList {
				for _, part := range strings.Split(value, ",") {
					part = strings.TrimSpace(part)
					if part != "" {
						opts.values[name] = append(opts.values[name], part)
					}
				}
			} else {
				if !spec.repeatable {
					opts.values[name] = nil
				}
				opts.values[name] = append(opts.values[name], value)
			}
		}
	}
	return opts, nil
}

// table is a tiny tabwriter wrapper used by the commands.
type table struct {
	w *tabwriter.Writer
}

func newTable(out io.Writer) *table {
	return &table{w: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}
}

func (t *table) header(cols ...string) { t.row(cols...) }
func (t *table) row(cols ...string) {
	cells := make([]string, len(cols))
	copy(cells, cols)
	fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}
func (t *table) flush() { _ = t.w.Flush() }

// Split tokenizes a command line for the interactive console.
func Split(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return args, nil
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a number")
	}
	return n, nil
}

// hostPlan describes the local capacity shown by `billing plan`.
type hostPlan struct {
	CPUs            int    `json:"cpus"`
	MemoryTotal     int64  `json:"memory_total"`
	MemoryAvailable int64  `json:"memory_available"`
	DiskAvailable   int64  `json:"disk_available"`
	Containers      int    `json:"containers"`
	Running         int    `json:"running"`
	DataDir         string `json:"data_dir"`
}

func inspectHost(cfg *config.Config) hostPlan {
	plan := hostPlan{
		CPUs:          1,
		DataDir:       cfg.DataDir,
		DiskAvailable: diskFree(cfg.DataDir),
	}
	plan.MemoryTotal, plan.MemoryAvailable = memoryInfo()
	if n, err := numCPU(); err == nil {
		plan.CPUs = n
	}
	return plan
}

func splitHostPort(addr string) (host string, port int, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("invalid address %q", addr)
	}
	port, err = strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q", addr)
	}
	return addr[:i], port, nil
}

// durationOf converts seconds to a time.Duration; used by tests.
func durationOf(seconds int64) time.Duration { return time.Duration(seconds) * time.Second }
