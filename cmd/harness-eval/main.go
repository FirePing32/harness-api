// Command harness-eval measures a running harness-api against a task suite.
//
//	harness-eval run -model gpt-4.1 -out baseline.json
//	harness-eval run -model gpt-4.1 -out candidate.json -label candidate
//	harness-eval compare baseline.json candidate.json
//	harness-eval list
//
// It needs a server and a model, because what it measures is a whole system
// doing real work. There is no offline mode and no mocked provider: a suite
// that can pass without a model is not measuring anything worth knowing.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FirePing32/harness-api/internal/eval"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "harness-eval: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}

	switch args[0] {
	case "run":
		return runSuite(args[1:])
	case "compare":
		return compare(args[1:])
	case "list":
		return list(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `harness-eval — measure a running harness-api

  run      execute the task suite and write a report
  compare  diff two reports
  list     show the tasks in a suite

Run "harness-eval <subcommand> -h" for flags.
`)
}

func runSuite(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)

	var (
		baseURL     string
		apiKey      string
		model       string
		tasksDir    string
		only        string
		label       string
		out         string
		workRoot    string
		reps        int
		concurrency int
		timeout     time.Duration
		minPassRate float64
		keep        bool
	)

	fs.StringVar(&baseURL, "base-url", envOr("HARNESS_EVAL_BASE_URL", "http://127.0.0.1:8080/v1"),
		"harness-api base URL")
	fs.StringVar(&apiKey, "api-key", os.Getenv("HARNESS_EVAL_API_KEY"),
		"bearer token, if the server requires one")
	fs.StringVar(&model, "model", os.Getenv("HARNESS_EVAL_MODEL"),
		"model to request; required, and recorded in the report")
	fs.StringVar(&tasksDir, "tasks", "evals/tasks", "task suite directory")
	fs.StringVar(&only, "only", "", "comma-separated task names to run")
	fs.StringVar(&label, "label", "", "name for this configuration, shown when comparing")
	fs.StringVar(&out, "out", "", "write the JSON report here")
	fs.StringVar(&workRoot, "work-root", "", "where per-run workspaces are created")
	fs.IntVar(&reps, "n", 3, "repetitions per task; agent runs are stochastic")
	fs.IntVar(&concurrency, "concurrency", 2, "repetitions in flight at once")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "per-repetition ceiling")
	fs.Float64Var(&minPassRate, "min-pass-rate", 0,
		"exit non-zero below this pass rate; off by default, because gating on a "+
			"stochastic suite fails builds for no reason")
	fs.BoolVar(&keep, "keep", false, "keep workspaces for inspection")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if model == "" {
		return fmt.Errorf("-model is required: a report that does not say which model " +
			"produced it cannot be compared against anything")
	}

	tasks, err := eval.LoadSuite(tasksDir, splitList(only))
	if err != nil {
		return err
	}

	// Ctrl-C stops scheduling and lets the in-flight runs finish rather than
	// abandoning half-written workspaces.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var mu sync.Mutex
	done := 0
	total := 0
	for _, t := range tasks {
		total += t.RepetitionsOr(reps)
	}

	runner := eval.NewRunner(eval.Options{
		Client:      eval.NewClient(baseURL, apiKey, model),
		WorkRoot:    workRoot,
		Repetitions: reps,
		Timeout:     timeout,
		Concurrency: concurrency,
		Keep:        keep,
		Progress: func(r eval.Run) {
			mu.Lock()
			defer mu.Unlock()
			done++
			fmt.Fprintf(os.Stderr, "[%*d/%d] %-26s rep %d  %s  %2d turns  %5.0fs\n",
				len(fmt.Sprint(total)), done, total, r.Task, r.Rep,
				outcome(r), r.Turns, float64(r.DurationMS)/1000)
		},
	})

	fmt.Fprintf(os.Stderr, "%d task(s), %d run(s), model %s\n\n", len(tasks), total, model)

	report := &eval.Report{
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Config: eval.Config{
			BaseURL: baseURL, Model: model, Label: label, Repetitions: reps,
		},
	}
	start := time.Now()

	for _, t := range tasks {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "\ninterrupted; reporting %d completed task(s)\n", len(report.Tasks))
			break
		}
		report.Tasks = append(report.Tasks, eval.Summarise(t, runner.RunTask(ctx, t)))
	}

	report.DurationMS = time.Since(start).Milliseconds()
	report.Finish()
	report.Print(os.Stdout)

	if out != "" {
		if err := report.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nreport written to %s\n", out)
	}

	if minPassRate > 0 && report.Measured > 0 && report.PassRate < minPassRate {
		return fmt.Errorf("pass rate %.0f%% is below the %.0f%% floor",
			report.PassRate*100, minPassRate*100)
	}
	return nil
}

func compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: harness-eval compare <before.json> <after.json>")
	}

	a, err := eval.LoadReport(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := eval.LoadReport(fs.Arg(1))
	if err != nil {
		return err
	}

	eval.Compare(a, b).Print(os.Stdout)
	return nil
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	tasksDir := fs.String("tasks", "evals/tasks", "task suite directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tasks, err := eval.LoadSuite(*tasksDir, nil)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		fmt.Printf("%-26s  %-22s  %s\n", t.Name, t.Category, firstLine(t.Notes))
	}
	fmt.Printf("\n%d task(s) in %s\n", len(tasks), *tasksDir)
	return nil
}

func outcome(r eval.Run) string {
	switch {
	case r.Errored():
		return "ERROR"
	case r.Passed:
		return "pass "
	default:
		return "FAIL "
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
