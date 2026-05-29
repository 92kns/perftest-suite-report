# perftest-suite-report

A Go CLI that generates a weekly HTML report of the top failing test suites across Mozilla's performance testing infrastructure. Inspired by [perftest-triage-report](https://github.com/92kns/perftest_triage_report), but suite-centric rather than bug-centric.

Instead of "here are the bugs and which suites they hit", this report asks: **which suites are failing the most, and what's driving it?**

## What it shows

- Top N failing test suites ranked by failure count, grouped by harness (Raptor, Talos, AWSY, mozperftest)
- 7-day primary window + 2-day snapshot side by side
- 🔺 **Spike flag** when a suite's last 2 days account for a disproportionate share of its weekly failures — useful for catching new regressions same-day
- Platform and repository (tree) breakdown per suite
- Contributing Bugzilla bugs linked back to bugzilla.mozilla.org, with per-bug failure counts

## Usage

```
go run . [flags]
```

Or build once and run the binary:

```
go build -o perftest-suite-report .
./perftest-suite-report [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--days` | `7` | Number of days back to query |
| `--top` | `20` | Number of top suites to include |
| `--concurrency` | `10` | Max concurrent Treeherder API requests |
| `--no-open` | `false` | Don't open the report in a browser after generating |

### Recommended daily cron invocation

```
./perftest-suite-report --days 7 --top 25 --concurrency 15 --no-open
```

## Output

Generates `report.html` in the current directory and opens it in your browser (unless `--no-open` is set).

Suites are grouped by harness and ranked within each group:

```
🔥 Top 25 Failing Test Suites (last 7 days)

Raptor — 450 failures (7d), 180 (2d)
  #1 raptor-tp6-cold-fenix-firefox   180 (7d)  72 (2d)  🔺 spiking
  #2 raptor-speedometer-firefox       90 (7d)  20 (2d)
  ...

Talos — 210 failures (7d), 60 (2d)
  #1 talos-tp5o                      120 (7d)  30 (2d)
  ...
```

Each suite expands to show platforms, repositories, and the Bugzilla bugs that contributed failures.

## Data sources

- **Bugzilla** (`bugzilla.mozilla.org/rest/bug`) — open intermittent-failure and Perma bugs across perf components: AWSY, Condprofile, mozperftest, Performance, Raptor, Talos
- **Treeherder** (`treeherder.mozilla.org/api/failuresbybug`) — per-job failure records keyed by bug, providing suite name, platform, and tree

## How the spike flag works

A suite is flagged as spiking when its 2-day failure count exceeds 3/7 (~43%) of its 7-day total — more than 1.5× the expected daily rate. Integer math: `twoDayFails × 7 > totalFails × 3`.
