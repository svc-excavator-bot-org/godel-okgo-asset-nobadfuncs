// Copyright 2016 Palantir Technologies, Inc.
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

package check

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/palantir/okgo/okgo"
)

func Run(projectParam okgo.ProjectParam, checkersToRun []okgo.CheckerType, pkgPaths []string, projectDir string, factory okgo.CheckerFactory, parallelism int, stdout io.Writer) error {
	var checkers []okgo.CheckerParam
	maxTypeLen := 0
	for _, checkerType := range checkersToRun {
		if len(checkerType) > maxTypeLen {
			maxTypeLen = len(checkerType)
		}
		param, ok := projectParam.Checks[checkerType]
		if ok {
			checkers = append(checkers, param)
			continue
		}
		checker, err := factory.NewChecker(checkerType, nil)
		if err != nil {
			return errors.Wrapf(err, "failed to create checkerType %s", checkerType)
		}
		checkers = append(checkers, okgo.CheckerParam{
			Checker: checker,
		})
	}

	var rErr error
	sort.Slice(checkers, func(i, j int) bool {
		iPriority, err := checkers[i].Checker.Priority()
		if err != nil && rErr == nil {
			rErr = err
		}
		jPriority, err := checkers[j].Checker.Priority()
		if err != nil && rErr == nil {
			rErr = err
		}
		return iPriority < jPriority
	})
	if rErr != nil {
		return errors.Wrapf(rErr, "failed to determine priority")
	}

	jobs := make(chan okgo.CheckerParam)
	results := make(chan checkResult, len(checkers))

	for w := 0; w < parallelism; w++ {
		go singleCheckWorker(pkgPaths, projectDir, maxTypeLen, parallelism > 1, jobs, results, stdout)
	}

	for _, checker := range checkers {
		jobs <- checker
	}
	close(jobs)

	failureExists := false
	for range checkers {
		checkResult := <-results
		if checkResult.producedOutput {
			failureExists = true
		}
	}
	if failureExists {
		// return empty failure to indicate non-zero exit code
		return fmt.Errorf("")
	}
	return nil
}

type checkResult struct {
	checkerType    okgo.CheckerType
	producedOutput bool
}

func singleCheckWorker(pkgPaths []string, projectDir string, maxTypeLen int, multipleWorkers bool, checkJobs <-chan okgo.CheckerParam, results chan<- checkResult, stdout io.Writer) {
	for checkerParam := range checkJobs {
		if checkerParam.Skip {
			results <- checkResult{}
			continue
		}

		checkerType, err := checkerParam.Checker.Type()
		if err != nil {
			fmt.Fprintf(stdout, "failed to determine type for Checker: %v", err)
			continue
		}
		prefixWithPadding := ""
		if multipleWorkers {
			prefixWithPadding = fmt.Sprintf("[%s] ", checkerType) + strings.Repeat(" ", maxTypeLen-len(checkerType))
		}
		results <- runCheck(checkerType, prefixWithPadding, checkerParam, pkgPaths, projectDir, stdout)
	}
}

func runCheck(checkerType okgo.CheckerType, outputPrefix string, checkerParam okgo.CheckerParam, pkgPaths []string, projectDir string, stdout io.Writer) checkResult {
	fmt.Fprintf(stdout, "%sRunning %s...\n", outputPrefix, checkerType)

	result := checkResult{
		checkerType: checkerType,
	}
	var filteredPkgPaths []string
	for _, pkgPath := range pkgPaths {
		if checkerParam.Exclude != nil && checkerParam.Exclude.Match(pkgPath) {
			// skip excludes
			continue
		}
		filteredPkgPaths = append(filteredPkgPaths, pkgPath)
	}

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(stdout, "%s%s\n", outputPrefix, "failed to create pipe")
		result.producedOutput = true
		return result
	}

	done := make(chan bool)

	go func() {
		scanner := bufio.NewScanner(pipeR)
		for scanner.Scan() {
			line := scanner.Text()
			issue := okgo.NewIssueFromJSON(line)

			if issue.Path != "" {
				// legitimate issue: determine whether or not it should be filtered out
				filterOut := false
				for _, filter := range checkerParam.Filters {
					if filter.Filter(issue) {
						filterOut = true
						break
					}
				}
				if filterOut {
					continue
				}
			}
			fmt.Fprintf(stdout, "%s%s\n", outputPrefix, strings.Replace(issue.String(), "\n", "\n"+outputPrefix, -1))
			result.producedOutput = true
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(stdout, "%s%s\n", outputPrefix, "scanner error encountered while reading output")
			result.producedOutput = true
		}
		done <- true
	}()

	// run check
	checkerParam.Checker.Check(filteredPkgPaths, projectDir, pipeW)

	if err := pipeW.Close(); err != nil {
		<-done
		fmt.Fprintf(stdout, "%s%s\n", outputPrefix, "failed to close pipe writer")
		result.producedOutput = true
		return result
	}

	// wait until all output has been read
	<-done

	fmt.Fprintf(stdout, "%sFinished %s\n", outputPrefix, checkerType)

	return result
}
