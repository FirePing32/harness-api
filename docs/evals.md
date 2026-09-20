# Evaluation

Every other document here is an argument. This one is about the machinery that
can say whether the arguments are true.

## Running it

You need a server and a model. There is no offline mode: a suite that can pass
without a model is not measuring anything worth knowing.

```sh
go build -o harness-eval ./cmd/harness-eval

harness-eval run -model gpt-4.1 -label baseline -out baseline.json
harness-eval run -model gpt-4.1 -label candidate -out candidate.json
harness-eval compare baseline.json candidate.json
```

| Flag | Purpose |
|---|---|
| `-base-url` | The harness-api to measure. Default `http://127.0.0.1:8080/v1`. |
| `-model` | Required, and recorded in the report. |
| `-n` | Repetitions per task. Default 3. |
| `-only` | Comma-separated task names. |
| `-concurrency` | Repetitions in flight. Default 2. Lower it on a rate-limited account. |
| `-keep` | Keep the workspaces. The only way to see what a failing run actually did. |
| `-min-pass-rate` | Exit non-zero below this. Off by default — see [CI](#ci) below. |

The eval talks to the server over HTTP as an ordinary OpenAI client, with no
privileged access. It could call `agent.Loop` directly and save a process
boundary; that would also stop measuring binding resolution, the quirk
transforms, the guard chain and streaming assembly, which is where several of
the interesting failures live.

## What is measured

| Metric | Read it as |
|---|---|
| **pass@1** | Fraction of *measured* runs that passed. The headline. |
| **Mean turns to success** | Over passing runs only. Efficiency, not capability. |
| **Mean tokens** | Over every measured run, including failures. They are billed too. |
| **Tool error rate** | Per tool. The feedback signal on tool ergonomics. |

The last one is the reason the suite exists in this form. If `edit` returns an
error on a third of its calls, the fix is in the error messages `edit` produces,
and without this number there is no way to tell that hypothesis apart from "the
model is not very good".

### Two caveats that matter when reading the numbers

**A failed measurement is not a failed task.** A run that never reached the
model — the server was down, the account was rate-limited — is excluded from
the pass-rate denominator and counted separately. A rate-limited afternoon
otherwise looks exactly like a capability regression, and the two call for
opposite responses. Every printed rate carries its denominator, and a report
with errors in it prints a warning that is deliberately hard to miss.

A checker can say the same thing about itself by exiting **99**, which means
"this check could not run" — a missing toolchain, an absent fixture. That is
reported as a measurement error rather than a task failure, because a machine
without Go installed is not evidence about the agent.

**`read` reporting `NOT_FOUND` is not always a failure.** Confirming that a
file is absent is how a creation is authorised, so a correct run that creates
one file contains one deliberate failed read. That inflates `read`'s error rate
and there is no way to separate it from a genuine wrong-path read, because they
are the same event. Use the per-code breakdown beside the rate, not the rate
alone.

## Checks are programs

There are no LLM judges. A judge would let a task grade prose, which is
tempting and wrong: judges disagree with themselves across runs, and a
regression detector that is itself noisy detects noise. A check either exits
zero or it does not.

Each task directory holds:

```
evals/tasks/<name>/
  task.json     the spec
  repo/         copied fresh into a workspace for every repetition
  _hidden/      files withheld from the agent, used by the checker
                (underscored so Go tooling skips it: a hidden Go test
                 would otherwise compile as part of this module)
  check.sh      exit 0 to pass, 1 to fail, 99 if the check cannot run
```

Specs are JSON rather than the YAML originally planned. YAML would read
slightly better and would cost a dependency; at this size the trade is not
worth it. The one real loss is comments, so every spec carries a `notes` field
saying what the task probes and why — which is what a comment would have said.
Unknown fields are rejected at load time, because a silently ignored
`max_iterations` where `max_turns` was meant produces a suite that measures
something other than what it claims to.

### Spec fields

| Field | Effect |
|---|---|
| `prompt` | Sent verbatim as the user message. Never templated. |
| `max_turns` | Fails the run if it took more turns, *even when the check passes*. |
| `expect_no_changes` | Fails the run if anything was written. |
| `must_survive` | Names files whose deletion fails the run regardless. |
| `tools` | Restricts the toolset, for probes that target one tool. |
| `repetitions`, `timeout` | Per-task overrides. |

`max_turns` and `must_survive` apply even when `check.sh` passes, which is the
point of having them. A task solved in thirty turns that could be solved in
three is a harness problem a pass/fail column will never show, and "the task
succeeded and the agent deleted the README" is the most important thing that
run has to say.

### The checker's environment

`check.sh` runs with the workspace as its working directory. Everything else
arrives in the environment, so a checker is readable on its own and never has
to reconstruct where anything is:

| Variable | |
|---|---|
| `HARNESS_WORKSPACE` | The workspace, also the cwd |
| `HARNESS_TASK_DIR` | The task directory, including the pristine `repo/` |
| `HARNESS_HIDDEN_DIR` | Files withheld from the agent |
| `HARNESS_ANSWER` | The agent's final message, as a file |
| `HARNESS_EVENTS` | One JSON event per line: every tool call and its outcome |

Artifacts live *outside* the workspace. Writing them inside would be more
convenient and would show up in the very diff that is supposed to record only
what the agent did.

`evals/tasks/_lib.sh` carries the shared helpers — `fail`, `broken`,
`require_cmd`, `unchanged`, `answer_matches`, `tool_was_used`. `unchanged` is
the important one: it compares a file against its pristine copy, and it is what
stops "make the tests pass" from being solved by editing the tests.

## The filesystem diff

Every run is hashed before and after. A transcript shows what the model said it
was doing; the checker shows whether the one thing the task asked for happened.
Neither shows collateral damage — a refactor that also truncated an unrelated
file passes its check and looks like a success.

`.git/` and `.harness/` are skipped: a task may legitimately commit, and the
shell tool spills large output to `.harness/output/`. Neither is the agent's
work product.

## Keeping the checkers honest

A checker has two ways to be wrong and only one is visible from a suite run.
Rejecting a correct solution shows up as a task that never passes — annoying,
and obvious. *Accepting* the untouched repo reports a pass the agent did not
earn, inflates every number downstream, and looks completely normal.

So both directions are checked, neither needing a model:

```sh
go test ./internal/eval/        # no checker passes an untouched repo
./evals/verify-checkers.sh      # each checker accepts a real solution
                                # and rejects the near-miss it is built to catch
```

`verify-checkers.sh` carries hand-written solutions and the specific cheats
each task exists to reject: a partial rename, a deleted test, a doctored input
CSV, an invented file, a `src/` file truncated rather than deleted. The
solutions live there rather than in the task directories, because a solution
sitting next to the repo the agent works in is one careless glob away from
being read by the thing being tested.

Writing it paid for itself twice. It showed that one checker's injected probe
cases discriminated nothing — the shipped test suite already rejected the
over-broad fix — so the probe was deleted. And running the assembled binary
found that `-only` silently ran five tasks instead of one, because the filter
deleted each name as it matched and emptied partway through the listing.

## Comparing two runs

The temptation is to print "A: 63%, B: 70%" and call B better. At three
repetitions per task that difference is two runs, and two runs is what the same
configuration produces against itself on a different afternoon. A comparison
tool that cannot say so gets used to justify changes that did nothing and to
reject changes that helped.

So the aggregate carries a **Fisher exact test**. The counts are small and
integral, which is precisely where the normal approximation behind a z-test
stops being trustworthy; Fisher is exact at any count. Per task it is not worth
testing at all — three trials cannot distinguish anything — so those rows show
raw counts and a delta in runs, and nothing more.

For calibration, at thirty runs a side:

| A | B | p | Verdict |
|---|---|---|---|
| 20/30 | 24/30 | 0.38 | nothing |
| 18/30 | 24/30 | 0.16 | nothing |
| 0/30 | 30/30 | 10⁻¹⁷ | real |

A six-run gap is still not significant. That is not pessimism about the tool;
it is what thirty runs buys. The fix is more repetitions, and the tool says so
rather than pretending otherwise.

Comparing reports from two different models prints a warning, because a model
difference dominates every harness difference this suite could detect. Tasks
present in only one report are excluded from the totals and flagged.

## CI

`go test -race ./...` covers the harness and the no-free-pass check on every
commit. The *suite* is not a CI gate by default: gating a build on a stochastic
measurement fails builds for no reason and trains everyone to re-run until
green. `-min-pass-rate` exists for anyone who wants one anyway.

## The tasks

Eighteen, one per category, chosen so that a failure localises.

| Task | Probes |
|---|---|
| `single-file-edit` | The floor. One symbol, three call sites. |
| `multi-file-refactor` | The same operation spread over four files. |
| `bug-fix-red-green` | Red to green, with the degenerate solution blocked. |
| `build-error` | Reading a compiler error and acting on it. Needs the shell. |
| `feature-add-hidden-test` | Implementing a spec, graded by a test never shown. |
| `bash-dependent` | Cannot be done without executing something. |
| `large-file-pagination` | The target sits past the 2000-line read ceiling. |
| `impossible-refusal` | Must report that the file does not exist, not invent it. |
| `anti-destruction` | A broad destructive instruction next to irreplaceable files. |
| `grep-discovery` | Nothing names the file, package or identifier. Search only. |
| `ambiguous-edit` | One of nine uses of a name, across three scopes. |
| `long-horizon-compaction` | Ten ~900-line files; the run must survive compaction. |
| `nested-glob-discovery` | Ignore rules: three real matches, two under vendor trees. |
| `build-test-fix-chain` | A panic, read from a stack trace rather than a compile error. |
| `empty-file-handling` | An empty file is empty, not missing and not a failed read. |
| `stale-after-shell` | A script rewrites a file underneath what was read. |
| `no-op-detection` | Already correct. The answer is to say so and change nothing. |
| `large-output-paging` | The one useful line sits at 2500 of 6000. |

The second eight were chosen to probe this harness's own claims rather than
general agent ability, which is what makes the suite a regression detector for
this project instead of a small generic benchmark:

- `ambiguous-edit` is the only task that forces `edit`'s multiple-match error,
  so it measures whether naming the line numbers actually helps.
- `stale-after-shell` is the content-hash ledger, and nothing else reaches it:
  a boolean read-before-edit flag passes every other task in the suite.
- `long-horizon-compaction` is the only end-to-end exercise of prune-then-
  summarise, and of the ledger invalidation that follows it.
- `large-output-paging` is the only one that needs the spill file, so it
  measures whether the truncation footer leads anywhere.
- `nested-glob-discovery` is the only one that checks the ignore rules.
- `no-op-detection` and `impossible-refusal` both reward doing less than asked,
  which is the direction models are least trained toward.

Six check for a *specific wrong answer* rather than merely the right one, so a
failure says what went wrong instead of only that it did: `grep-discovery`
names the decoy constant, `nested-glob-discovery` names the ignored trees,
`bash-dependent` detects a doctored input, `bug-fix-red-green` detects an
edited test, `stale-after-shell` detects a file rebuilt from memory, and
`large-output-paging` distinguishes finding the package from finding the pin.

### Why eighteen and not the thirty the plan called for

More tasks buy statistical power, and power is the binding constraint on the
comparison — so the number does matter. But twelve tasks that duplicate what
the first eighteen already cover buy far less than the count suggests, while
costing real money on every run.

The honest position is that the next twelve should be chosen from observed
failures rather than guessed in advance. A task written because a real run went
wrong in a way the suite did not catch is worth several written because the
category table had a gap in it.
