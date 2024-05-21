// Copyright © 2016 Phil Estes <estesp@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/estesp/bucketbench/benches"
	"github.com/estesp/bucketbench/driver"
	"github.com/montanaflynn/stats"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"
)

const (
	defaultLimitThreads = 10
	defaultLimitIter    = 1000
	limitBenchmarkName  = "Limit"
)

var (
	yamlFile  string
	trace     bool
	skipLimit bool
	overhead  bool
	legacy    bool
)

// simple structure to handle collecting output data which will be displayed
// after all benchmarks are complete
type benchResult struct {
	name        string
	driverInfo  string
	threads     int
	iterations  int
	threadRates []float64
	statistics  [][]benches.RunStatistics
}

// simple structure to handle collecting output data which will be displayed
// after one iteration benchmark is complete
type benchSingleResult struct {
	name       string
	benchInfo  string
	driverInfo string
	threadRate float64
	statistic  []benches.RunStatistics
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the benchmark against the selected container engine components",
	Long: `The YAML file provided via the --benchmark flag will determine which
lifecycle container commands to run against which container runtimes, specifying
iterations and number of concurrent threads. Results will be displayed afterwards.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stopC := make(chan os.Signal, 1)
		signal.Notify(stopC, os.Interrupt, syscall.SIGTERM)

		go func() {
			select {
			case <-stopC:
				cancel()
			case <-ctx.Done():
				return
			}
		}()

		if yamlFile == "" {
			return fmt.Errorf("No YAML file provided with --benchmark/-b; nothing to do")
		}
		benchmark, err := readYaml(yamlFile)
		if err != nil {
			return fmt.Errorf("Error reading benchmark file %q: %v", yamlFile, err)
		}
		// verify that an image name exists in the benchmark as
		// we'll end up erroring out further down if no image is
		// specified
		if benchmark.Image == "" {
			return fmt.Errorf("Please provide an 'image:' entry in your benchmark YAML")
		}

		var (
			maxThreads = defaultLimitThreads
			results    []benchResult
		)
		if !skipLimit {
			// get thread limit stats
			limitRates := runLimitTest(ctx)
			limitResult := benchResult{
				name:        limitBenchmarkName,
				threads:     defaultLimitThreads,
				iterations:  defaultLimitIter,
				threadRates: limitRates,
			}
			results = append(results, limitResult)
		} else {
			maxThreads = 0 // no limit results in output
		}

		benchType := benches.Custom
		if overhead {
			benchType = benches.Overhead
		}

		for _, driverEntry := range benchmark.Drivers {
			result, err := runBenchmark(ctx, benchType, driverEntry, benchmark, legacy)
			if err != nil {
				return err
			}
			results = append(results, result)
			maxThreads = intMax(maxThreads, driverEntry.Threads)
		}

		// output benchmark results
		outputRunDetails(maxThreads, results, overhead, legacy)

		log.Info("Benchmark runs complete")
		return nil
	},
}

func runLimitTest(ctx context.Context) []float64 {
	var rates []float64
	// get thread limit stats
	for i := 1; i <= defaultLimitThreads; i++ {
		limit, _ := benches.New(benches.Limit, &benches.DriverConfig{})
		limit.Init(ctx, "", driver.Null, "", "", "", trace)
		limit.Run(ctx, i, defaultLimitIter, 0, nil)
		duration := limit.Elapsed()
		rate := float64(i*defaultLimitIter) / duration.Seconds()
		rates = append(rates, rate)
		log.Infof("Limit: threads %d, iterations %d, rate: %6.2f", i, defaultLimitIter, rate)
	}
	return rates
}

func runBenchmark(ctx context.Context, benchType benches.Type, driverConfig benches.DriverConfig, benchmark benches.Benchmark, legacyMode bool) (benchResult, error) {
	var (
		rates      []float64
		stats      [][]benches.RunStatistics
		benchInfo  string
		driverInfo string
	)

	if driverConfig.Extended != nil {
		ctx = context.WithValue(ctx, "extended", driverConfig.Extended)
	}

	if legacyMode {
		stats = make([][]benches.RunStatistics, driverConfig.Threads)
		// Legacy mode in total run N test suites. for each test suite, it runs with n thread and n is the current thread numbers.
		for i := 1; i <= driverConfig.Threads; i++ {
			singleResult, err := runBenchmarkOnce(ctx, benchType, driverConfig, benchmark, i)
			if err != nil {
				return benchResult{}, err
			}
			benchInfo, driverInfo = singleResult.benchInfo, singleResult.driverInfo
			rates = append(rates, singleResult.threadRate)
			stats[i-1] = singleResult.statistic
		}
	} else {
		stats = make([][]benches.RunStatistics, 1)
		singleResult, err := runBenchmarkOnce(ctx, benchType, driverConfig, benchmark, driverConfig.Threads)
		if err != nil {
			return benchResult{}, err
		}
		benchInfo, driverInfo = singleResult.benchInfo, singleResult.driverInfo
		rates = append(rates, singleResult.threadRate)
		stats[0] = singleResult.statistic
	}

	result := benchResult{
		name:        benchInfo,
		driverInfo:  driverInfo,
		threads:     driverConfig.Threads,
		iterations:  driverConfig.Iterations,
		threadRates: rates,
		statistics:  stats,
	}

	return result, nil
}

// runBenchmark run exact one test suite
func runBenchmarkOnce(ctx context.Context, benchType benches.Type, driverConfig benches.DriverConfig, benchmark benches.Benchmark, threads int) (benchSingleResult, error) {
	bench, err := benches.New(benchType, &driverConfig)
	if err != nil {
		return benchSingleResult{}, err
	}

	driverType := driver.StringToType(driverConfig.Type)
	imageInfo := benchmark.Image
	if driverType == driver.Runc || driverType == driver.Ctr || driverType == driver.CRun || driverType == driver.Youki {
		// legacy ctr mode, runc, crun and youki drivers need an exploded rootfs
		// first, verify that a rootfs was provided in the benchmark YAML
		if benchmark.RootFs == "" {
			return benchSingleResult{}, fmt.Errorf("no rootfs defined in the benchmark YAML; driver %s requires a root FS path", driverConfig.Type)
		}

		imageInfo = benchmark.RootFs
	}

	err = bench.Init(ctx, benchmark.Name, driverType, driverConfig.ClientPath, imageInfo, benchmark.Command, trace)
	if err != nil {
		return benchSingleResult{}, err
	}

	benchInfo := fmt.Sprintf("%s:%s", benchType, driverConfig.Type)

	if err = bench.Validate(ctx); err != nil {
		return benchSingleResult{}, fmt.Errorf("error during bench validate: %v", err)
	}

	info, err := bench.Info(ctx)
	if err != nil {
		return benchSingleResult{}, errors.Wrap(err, "failed to query driver info")
	}

	driverInfo := info

	err = bench.Run(ctx, threads, driverConfig.Iterations, driverConfig.Duration, benchmark.Commands)
	if err != nil {
		return benchSingleResult{}, fmt.Errorf("error during bench run: %v", err)
	}

	duration := bench.Elapsed()
	rate := float64(threads*driverConfig.Iterations) / duration.Seconds()

	result := benchSingleResult{
		name:       benchInfo,
		driverInfo: driverInfo,
		benchInfo:  benchInfo,
		threadRate: rate,
		statistic:  bench.Stats(),
	}

	log.Infof("%s: threads %d, iterations %d, rate: %6.2f", benchInfo, threads, driverConfig.Iterations, rate)
	return result, nil
}

func getDelta(before, after float64) float64 {
	switch {
	case before != 0:
		return after / before
	case after == 0:
		return 1
	default:
		return math.Inf(1)
	}
}

func outputRunDetails(maxThreads int, results []benchResult, overhead bool, legacyMode bool) {
	w := tabwriter.NewWriter(os.Stdout, 10, 4, 2, ' ', tabwriter.AlignRight)

	fmt.Printf("\nSUMMARY TIMINGS/THREAD RATES\n\n")
	fmt.Fprintf(w, " \tIter/Thd\t1 thrd")
	for i := 2; i <= maxThreads; i++ {
		fmt.Fprintf(w, "\t%d thrds", i)
	}
	fmt.Fprintln(w, "\t ")

	for _, result := range results {
		if legacyMode {
			outputThreadRatesLegacy(w, result)
		} else {
			outputThreadRates(w, result)
		}
	}
	w.Flush()
	fmt.Println("")

	cmdList := []string{"run", "pause", "resume", "stop", "delete"}
	fmt.Printf("DETAILED COMMAND TIMINGS/STATISTICS\n")
	// output per-command timings across the runs as well
	for _, result := range results {
		// only 1 result
		if result.name == limitBenchmarkName {
			// the limit "benchmark" has no detailed statistics
			continue
		}
		if legacyMode {
			outputDetailCommandStatsLegacy(result, w, cmdList)
		} else {
			outputDetailCommandStats(result, w, cmdList)
		}

		fmt.Println("")
	}

	w.Flush()

	if overhead {
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "OVERHEAD\n")

		var overheadResults []benchResult
		for _, res := range results {
			if res.name == limitBenchmarkName {
				continue
			}
			overheadResults = append(overheadResults, res)
		}

		if len(overheadResults) == 0 {
			fmt.Fprint(w, "No data")
			return
		}

		// Preprocess statistics before output
		metrics := make([][]metricsResults, len(overheadResults))
		for i, res := range overheadResults {
			metrics[i] = make([]metricsResults, res.threads)
			for j := 0; j < len(res.statistics); j++ {
				metrics[i][j] = parseMetrics(res.statistics[j])
			}
		}

		for i, res := range overheadResults {

			fmt.Fprintf(w, "\n%s\n\n", res.driverInfo)

			fmt.Fprintf(w, "Bench / driver / threads\tMin\tMax\tAvg\tMin\tMax\tAvg\tMem %%\tCPU x\t\n")
			var size = res.threads
			if !legacyMode {
				size = 1
			}

			for j := 0; j < size; j++ {
				m := metrics[i][j]

				fmt.Fprintf(w,
					"%s:%d\t%d MB\t%d MB\t%d MB\t%.2f %%\t%.2f %%\t%.2f %%\t",
					res.name, j+1,
					m.minMem, m.maxMem, m.avgMem,
					m.minCPU, m.maxCPU, m.avgCPU)

				if i > 0 {
					// Output overhead comparing to first result

					if j < overheadResults[0].threads {
						// Mem percent change, ranging from -100% up.
						mem := 100*getDelta(float64(metrics[0][j].avgMem), float64(m.avgMem)) - 100
						cpu := getDelta(metrics[0][j].avgCPU, m.avgCPU)

						fmt.Fprintf(w, "%+.2f%%\t%.2fx\t", mem, cpu)
					}
				}

				fmt.Fprint(w, "\n")
			}
		}

		w.Flush()
	}
}

func outputDetailCommandStatsLegacy(result benchResult, w *tabwriter.Writer, cmdList []string) {
	for i := 0; i < result.threads; i++ {
		fmt.Fprintf(w, "%s:%d\tMin\tMax\tAvg\tMedian\tStddev\tErrors\tCancelled\tRate\t\n", result.name, i+1)
		cmdTimings := parseStats(result.statistics[i])
		nums := 0
		for _, stat := range result.statistics[i] {
			if stat.Daemon == nil {
				nums += 1
			}
		}
		// given we are working with a map, but we want consistent ordering in the output
		// we walk a slice of commands in a natural/expected order and output stats for
		// those that were used during the specific run
		for _, cmd := range cmdList {
			if stats, ok := cmdTimings[cmd]; ok {
				fmt.Fprintf(w, "%s\t%6.2f\t%6.2f\t%6.2f\t%6.2f\t%6.2f\t%d\t%d/%d\t%.2f\t\n", cmd, stats.min, stats.max, stats.avg, stats.median, stats.stddev, stats.errors,
					result.threads*result.iterations-nums, result.threads*result.iterations,
					((float64)(nums-stats.errors)/float64(result.threads*result.iterations))*100)
			}
		}
	}
}

func outputDetailCommandStats(result benchResult, w *tabwriter.Writer, cmdList []string) {
	fmt.Fprintf(w, "%s:%d\tMin\tMax\tAvg\tMedian\tStddev\tErrors\tCancelled\tRate\t\n", result.name, result.threads)
	cmdTimings := parseStats(result.statistics[0])
	nums := 0
	for _, stat := range result.statistics[0] {
		if stat.Daemon == nil {
			nums += 1
		}
	}
	for _, cmd := range cmdList {
		if stats, ok := cmdTimings[cmd]; ok {
			fmt.Fprintf(w, "%s\t%6.2f\t%6.2f\t%6.2f\t%6.2f\t%6.2f\t%d\t%d/%d\t%.2f\t\n", cmd, stats.min, stats.max, stats.avg, stats.median, stats.stddev, stats.errors,
				result.threads*result.iterations-nums, result.threads*result.iterations,
				((float64)(nums-stats.errors)/float64(result.threads*result.iterations))*100)
		}
	}
}

func outputThreadRates(w *tabwriter.Writer, result benchResult) {
	if result.name == limitBenchmarkName {
		outputThreadRatesLegacy(w, result)
		return
	}

	fmt.Fprintf(w, "%s\t%d", result.name, result.iterations)
	for i := 1; i <= result.threads; i++ {
		fmt.Fprintf(w, "\t")
	}
	fmt.Fprintf(w, "%7.2f\t ", result.threadRates[0])
}

func outputThreadRatesLegacy(w *tabwriter.Writer, result benchResult) {
	fmt.Fprintf(w, "%s\t%d\t%7.2f", result.name, result.iterations, result.threadRates[0])
	for i := 1; i < result.threads; i++ {
		fmt.Fprintf(w, "\t%7.2f", result.threadRates[i])
	}
	fmt.Fprintln(w, "\t ")
}

type metricsResults struct {
	minMem uint64
	maxMem uint64
	avgMem uint64
	minCPU float64
	maxCPU float64
	avgCPU float64
}

func parseMetrics(metrics []benches.RunStatistics) metricsResults {
	var mems []float64
	var cpus []float64

	metrics = filterStats(metrics, func(stat benches.RunStatistics) bool {
		return stat.Daemon != nil
	})

	for _, m := range metrics {
		mems = append(mems, float64(m.Daemon.Mem))
		cpus = append(cpus, m.Daemon.CPU)
	}

	minMem, err := stats.Min(mems)
	if err != nil {
		log.Errorf("error finding min mem: %v", err)
	}

	maxMem, err := stats.Max(mems)
	if err != nil {
		log.Errorf("error finding max mem: %v", err)
	}

	avgMem, err := stats.Mean(mems)
	if err != nil {
		log.Errorf("error finding avg mem: %v", err)
	}

	minCPU, err := stats.Min(cpus)
	if err != nil {
		log.Errorf("error finding min cpu: %v", err)
	}

	maxCPU, err := stats.Max(cpus)
	if err != nil {
		log.Errorf("error finding max cpu: %v", err)
	}

	avgCPU, err := stats.Mean(cpus)
	if err != nil {
		log.Errorf("error finding avg cpu: %v", err)
	}

	return metricsResults{
		minMem: uint64(minMem),
		maxMem: uint64(maxMem),
		avgMem: uint64(avgMem),
		minCPU: minCPU,
		maxCPU: maxCPU,
		avgCPU: avgCPU,
	}
}

type statResults struct {
	min    float64
	max    float64
	avg    float64
	median float64
	stddev float64
	errors int
}

func filterStats(stats []benches.RunStatistics, check func(benches.RunStatistics) bool) (ret []benches.RunStatistics) {
	for _, stat := range stats {
		if check(stat) {
			ret = append(ret, stat)
		}
	}

	return
}

func parseStats(statistics []benches.RunStatistics) map[string]statResults {
	result := make(map[string]statResults)
	durationSeq := make(map[string][]float64)
	errorSeq := make(map[string][]int)

	statistics = filterStats(statistics, func(stat benches.RunStatistics) bool {
		return stat.Daemon == nil
	})

	iterations := len(statistics)

	durationKeys := make([]string, len(statistics[0].Durations))
	i := 0
	for k := range statistics[0].Durations {
		durationKeys[i] = k
		i++
	}
	for i := 0; i < iterations; i++ {
		for key, duration := range statistics[i].Durations {
			durationSeq[key] = append(durationSeq[key], float64(duration.Nanoseconds()/int64(time.Millisecond)))
		}
		for key, errors := range statistics[i].Errors {
			errorSeq[key] = append(errorSeq[key], errors)
		}
	}
	for _, key := range durationKeys {
		// take the durations for this key and perform
		// several math/statistical functions:
		min, err := stats.Min(durationSeq[key])
		if err != nil {
			log.Errorf("Error finding stats.Min(): %v", err)
		}
		max, err := stats.Max(durationSeq[key])
		if err != nil {
			log.Errorf("Error finding stats.Max(): %v", err)
		}
		average, err := stats.Mean(durationSeq[key])
		if err != nil {
			log.Errorf("Error finding stats.Average(): %v", err)
		}
		median, err := stats.Median(durationSeq[key])
		if err != nil {
			log.Errorf("Error finding stats.Median(): %v", err)
		}
		stddev, err := stats.StandardDeviation(durationSeq[key])
		if err != nil {
			log.Errorf("Error finding stats.StdDev(): %v", err)
		}
		var errors int
		if errorSlice, ok := errorSeq[key]; ok {
			errors = intSum(errorSlice)
		}
		result[key] = statResults{
			min:    min,
			max:    max,
			avg:    average,
			median: median,
			stddev: stddev,
			errors: errors,
		}
	}
	return result
}

func intSum(slice []int) int {
	var total int
	for _, val := range slice {
		total += val
	}
	return total
}
func intMax(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func readYaml(filename string) (benches.Benchmark, error) {
	var benchmarkYaml benches.Benchmark
	yamlFile, err := os.ReadFile(filename)
	if err != nil {
		return benchmarkYaml, fmt.Errorf("Can't read YAML file %q: %v", filename, err)
	}
	err = yaml.Unmarshal(yamlFile, &benchmarkYaml)
	if err != nil {
		return benchmarkYaml, fmt.Errorf("Can't unmarshal YAML file %q: %v", filename, err)
	}
	return benchmarkYaml, nil
}

func init() {
	RootCmd.AddCommand(runCmd)
	runCmd.PersistentFlags().StringVarP(&yamlFile, "benchmark", "b", "", "YAML file with benchmark definition")
	runCmd.PersistentFlags().BoolVarP(&trace, "trace", "t", false, "Enable per-container tracing during benchmark runs")
	runCmd.PersistentFlags().BoolVarP(&skipLimit, "skip-limit", "s", false, "Skip 'limit' benchmark run")
	runCmd.PersistentFlags().BoolVarP(&overhead, "overhead", "o", false, "Output daemon overhead")
	runCmd.PersistentFlags().BoolVarP(&legacy, "legacy", "l", false, "legacy mode will run benchmark from 1 to N(thread number) iterations.")
}
