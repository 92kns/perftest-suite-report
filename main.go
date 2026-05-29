package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	BugzillaURL   = "https://bugzilla.mozilla.org/rest/bug"
	TreeherderURL = "https://treeherder.mozilla.org/api"
	outputHTML    = "report.html"
)

var components = []string{"AWSY", "Condprofile", "mozperftest", "Performance", "Raptor", "Talos"}

var (
	topN          int
	daysBack      int
	maxConcurrent int
	bugzillaBase  = BugzillaURL
	thBase        = TreeherderURL
)

//go:embed template.html
var reportTemplate string

// ===================== API types =====================

type THJobFailure struct {
	Platform  string `json:"platform"`
	Tree      string `json:"tree"`
	TestSuite string `json:"test_suite"`
}

type Bug struct {
	ID        int    `json:"id"`
	Summary   string `json:"summary"`
	Component string `json:"component"`
}

type BugListResponse struct {
	Bugs []Bug `json:"bugs"`
}

// ===================== Report types =====================

type BugContribution struct {
	ID        int
	Link      string
	GraphLink string
	Summary   string
	Component string
	Failures  int
}

type SuiteResult struct {
	Rank          int
	Suite         string
	TotalFailures int
	TwoDayFails   int
	Spike         bool // 2d share is >43% of 7d (1.5× the expected 2/7 rate)
	Platforms     []string
	Trees         []string
	Bugs          []BugContribution
}

type HarnessGroup struct {
	Name          string
	TotalFailures int
	TwoDayFails   int
	Suites        []SuiteResult
}

type reportData struct {
	Harnesses []HarnessGroup
	Generated string
	DaysBack  int
	TopN      int
}

// ===================== HTTP =====================

var httpClient = &http.Client{Timeout: 60 * time.Second}
var retrySleep = func(d time.Duration) { time.Sleep(d) }

func get(u string) (*http.Response, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			retrySleep(time.Duration(1<<uint(attempt-1)) * time.Second)
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "mozilla-perftest-suite-report/1.0")
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("request failed (attempt %d/3): %v", attempt+1, err)
			continue
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("status %s", resp.Status)
			log.Printf("server error (attempt %d/3): %s", attempt+1, resp.Status)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// ===================== Fetchers =====================

func fetchBugList(params url.Values) []Bug {
	resp, err := get(bugzillaBase + "?" + params.Encode())
	if err != nil {
		log.Printf("fetch bugs: %v", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out BugListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Printf("decode bugs: %v", err)
		return nil
	}
	return out.Bugs
}

func fetchAllBugs() []Bug {
	baseParams := func() url.Values {
		p := url.Values{}
		p.Set("product", "Testing")
		p.Set("resolution", "---")
		p.Set("keywords", "intermittent-failure")
		p.Set("keywords_type", "allwords")
		p.Set("include_fields", "id,summary,component")
		for _, c := range components {
			p.Add("component", c)
		}
		return p
	}

	p1 := baseParams()

	p2 := baseParams()
	p2.Set("short_desc", "Perma")
	p2.Set("short_desc_type", "allwordssubstr")

	var mu sync.Mutex
	var all []Bug
	var wg sync.WaitGroup
	wg.Add(2)
	for _, p := range []url.Values{p1, p2} {
		go func(params url.Values) {
			defer wg.Done()
			bugs := fetchBugList(params)
			mu.Lock()
			all = append(all, bugs...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	seen := map[int]bool{}
	deduped := all[:0]
	for _, b := range all {
		if !seen[b.ID] {
			seen[b.ID] = true
			deduped = append(deduped, b)
		}
	}
	return deduped
}

func fetchRawBreakdown(bugID int, start, end string) []THJobFailure {
	u := fmt.Sprintf("%s/failuresbybug/?startday=%s&endday=%s&tree=all&bug=%d", thBase, start, end, bugID)
	resp, err := get(u)
	if err != nil {
		log.Printf("fetch breakdown bug %d: %v", bugID, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var out []THJobFailure
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		log.Printf("decode breakdown bug %d: %v", bugID, err)
		return nil
	}
	return out
}

// ===================== Platform normalization =====================

func normalizePlatform(platform string) string {
	p := strings.ToLower(platform)
	if p == "" {
		return ""
	}
	base := strings.SplitN(p, "-", 2)[0]
	switch {
	case strings.HasPrefix(base, "android"):
		parts := strings.Split(p, "-")
		if len(parts) >= 3 {
			return strings.Join(parts[:3], "-")
		}
		return "android"
	case strings.HasPrefix(base, "linux"), strings.HasPrefix(base, "macosx"),
		strings.HasPrefix(base, "osx"), strings.HasPrefix(base, "win"):
		return base
	}
	switch {
	case strings.Contains(p, "android"):
		return "android"
	case strings.Contains(p, "linux"):
		return "linux"
	case strings.Contains(p, "macos"), strings.Contains(p, "osx"):
		return "macos"
	case strings.Contains(p, "win"):
		return "windows"
	}
	return platform
}

// ===================== Aggregation =====================

type suiteAgg struct {
	bugs      map[int]int
	platforms map[string]int
	trees     map[string]int
}

func aggregateBySuite(bugs []Bug, start, end string, sema chan struct{}) map[string]*suiteAgg {
	var mu sync.Mutex
	var wg sync.WaitGroup
	result := map[string]*suiteAgg{}

	for _, b := range bugs {
		wg.Add(1)
		sema <- struct{}{}
		go func(bug Bug) {
			defer wg.Done()
			defer func() { <-sema }()

			failures := fetchRawBreakdown(bug.ID, start, end)
			if len(failures) == 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, f := range failures {
				suite := f.TestSuite
				if suite == "" {
					continue
				}
				if _, ok := result[suite]; !ok {
					result[suite] = &suiteAgg{
						bugs:      map[int]int{},
						platforms: map[string]int{},
						trees:     map[string]int{},
					}
				}
				result[suite].bugs[bug.ID]++
				if p := normalizePlatform(f.Platform); p != "" {
					result[suite].platforms[p]++
				}
				if f.Tree != "" {
					result[suite].trees[f.Tree]++
				}
			}
		}(b)
	}
	wg.Wait()
	return result
}

func sortedCountStrs(m map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = fmt.Sprintf("%s: %d", p.k, p.v)
	}
	return out
}

// ===================== Classification =====================

func classifyHarness(suite string) string {
	s := strings.ToLower(suite)
	switch {
	case strings.HasPrefix(s, "raptor-"), strings.HasPrefix(s, "browsertime-"):
		return "Raptor"
	case strings.HasPrefix(s, "talos-"):
		return "Talos"
	case strings.HasPrefix(s, "awsy-"):
		return "AWSY"
	case strings.HasPrefix(s, "perftest-"), strings.HasPrefix(s, "mozperftest-"):
		return "mozperftest"
	default:
		return "Other"
	}
}

// isSpike returns true when the 2-day failure share is more than 1.5× the expected 2/7 rate,
// i.e. twoDayFails/totalFailures > 3/7 ≈ 0.43. Avoids float division.
func isSpike(totalFailures, twoDayFails int) bool {
	if totalFailures == 0 {
		return false
	}
	return twoDayFails*7 > totalFailures*3
}

// ===================== Build results =====================

func buildResults(bugs []Bug, current, twoday map[string]*suiteAgg, start, end string) []SuiteResult {
	bugByID := map[int]Bug{}
	for _, b := range bugs {
		bugByID[b.ID] = b
	}

	var results []SuiteResult
	for suite, data := range current {
		total := 0
		for _, c := range data.bugs {
			total += c
		}

		twoDayTotal := 0
		if td, ok := twoday[suite]; ok {
			for _, c := range td.bugs {
				twoDayTotal += c
			}
		}

		type bc struct{ id, count int }
		bcs := make([]bc, 0, len(data.bugs))
		for id, count := range data.bugs {
			bcs = append(bcs, bc{id, count})
		}
		sort.Slice(bcs, func(i, j int) bool { return bcs[i].count > bcs[j].count })

		contributions := make([]BugContribution, 0, len(bcs))
		for _, b := range bcs {
			bug := bugByID[b.id]
			contributions = append(contributions, BugContribution{
				ID:        bug.ID,
				Link:      fmt.Sprintf("https://bugzilla.mozilla.org/show_bug.cgi?id=%d", bug.ID),
				GraphLink: fmt.Sprintf("https://treeherder.mozilla.org/intermittent-failures/bugdetails?startday=%s&endday=%s&tree=all&bug=%d", start, end, bug.ID),
				Summary:   bug.Summary,
				Component: bug.Component,
				Failures:  b.count,
			})
		}

		results = append(results, SuiteResult{
			Suite:         suite,
			TotalFailures: total,
			TwoDayFails:   twoDayTotal,
			Spike:         isSpike(total, twoDayTotal),
			Platforms:     sortedCountStrs(data.platforms),
			Trees:         sortedCountStrs(data.trees),
			Bugs:          contributions,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].TotalFailures > results[j].TotalFailures
	})
	if len(results) > topN {
		results = results[:topN]
	}
	for i := range results {
		results[i].Rank = i + 1
	}
	return results
}

// groupByHarness groups a flat ranked suite list into harness sections.
// Global rank is preserved so #1 is still the worst suite across all harnesses.
// Groups are sorted by their combined failure count (hottest harness first).
func groupByHarness(suites []SuiteResult) []HarnessGroup {
	m := map[string]*HarnessGroup{}
	for i := range suites {
		h := classifyHarness(suites[i].Suite)
		if _, ok := m[h]; !ok {
			m[h] = &HarnessGroup{Name: h}
		}
		g := m[h]
		g.Suites = append(g.Suites, suites[i])
		g.TotalFailures += suites[i].TotalFailures
		g.TwoDayFails += suites[i].TwoDayFails
	}

	groups := make([]HarnessGroup, 0, len(m))
	for _, g := range m {
		for i := range g.Suites {
			g.Suites[i].Rank = i + 1
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].TotalFailures > groups[j].TotalFailures
	})
	return groups
}

// ===================== HTML =====================

func writeReport(harnesses []HarnessGroup) {
	total := 0
	for _, h := range harnesses {
		total += h.TotalFailures
	}
	_ = total

	data := reportData{
		Harnesses: harnesses,
		Generated: time.Now().UTC().Format("2006-01-02 15:04 MST"),
		DaysBack:  daysBack,
		TopN:      topN,
	}
	f, err := os.Create(outputHTML)
	if err != nil {
		log.Fatalf("create report: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := renderHTML(f, reportTemplate, data); err != nil {
		log.Fatalf("render: %v", err)
	}
}

func renderHTML(w io.Writer, tmpl string, data any) error {
	t := template.Must(template.New("report").Parse(tmpl))
	return t.Execute(w, data)
}

// ===================== Browser =====================

func openInBrowser(file string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", file)
	case "linux":
		cmd = exec.Command("xdg-open", file)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", file)
	default:
		fmt.Printf("Open %s in your browser.\n", file)
		return
	}
	_ = cmd.Start()
}

// ===================== Main =====================

func main() {
	start := time.Now()
	defer func() { fmt.Printf("⏱  Report generated in %s\n", time.Since(start)) }()

	noOpen := flag.Bool("no-open", false, "Disable opening browser after generating report")
	flag.IntVar(&topN, "top", 20, "Number of top suites to show")
	flag.IntVar(&daysBack, "days", 7, "Number of days back to query")
	flag.IntVar(&maxConcurrent, "concurrency", 10, "Maximum concurrent Treeherder fetches")
	flag.Parse()

	fmt.Println("Generating PerfTest suite failure report...")

	startDay := time.Now().AddDate(0, 0, -daysBack).Format("2006-01-02")
	endDay := time.Now().Format("2006-01-02")
	twoDayStart := time.Now().AddDate(0, 0, -2).Format("2006-01-02")

	bugs := fetchAllBugs()
	fmt.Printf("Found %d perf-related bugs\n", len(bugs))

	sema := make(chan struct{}, maxConcurrent)

	var currentAgg, twoDayAgg map[string]*suiteAgg
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); currentAgg = aggregateBySuite(bugs, startDay, endDay, sema) }()
	go func() { defer wg.Done(); twoDayAgg = aggregateBySuite(bugs, twoDayStart, endDay, sema) }()
	wg.Wait()

	results := buildResults(bugs, currentAgg, twoDayAgg, startDay, endDay)
	if len(results) == 0 {
		fmt.Println("No suite failures found.")
		return
	}

	harnesses := groupByHarness(results)
	fmt.Printf("Found failures across %d harnesses (%d suites)\n", len(harnesses), len(results))

	writeReport(harnesses)
	fmt.Println("✅ Report written to", outputHTML)
	if !*noOpen {
		openInBrowser(outputHTML)
	}
}
