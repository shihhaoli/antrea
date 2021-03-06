// Copyright 2019 Antrea Authors
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

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vmware-tanzu/antrea/pkg/agent/config"
)

const (
	ipfixCollectorImage = "antrea/ipfix-collector:06252020.1"
	ipfixCollectorPort  = "4739"
)

func skipIfNotBenchmarkTest(tb testing.TB) {
	if !testOptions.withBench {
		tb.Skipf("Skipping benchmark test: %s", tb.Name())
	}
}

func skipIfProviderIs(tb testing.TB, name string, reason string) {
	if testOptions.providerName == name {
		tb.Skipf("Skipping test for the '%s' provider: %s", name, reason)
	}
}

func skipIfNumNodesLessThan(tb testing.TB, required int) {
	if clusterInfo.numNodes < required {
		tb.Skipf("Skipping test as it requires %d different Nodes but cluster only has %d", required, clusterInfo.numNodes)
	}
}

func skipIfRunCoverage(tb testing.TB, reason string) {
	if testOptions.enableCoverage {
		tb.Skipf("Skipping test for the '%s' when run coverage: %s", tb.Name(), reason)
	}
}

func skipIfEncapModeIs(tb testing.TB, data *TestData, encapModes []config.TrafficEncapModeType) {
	currentEncapMode, err := data.GetEncapMode()
	if err != nil {
		tb.Fatalf("Failed to get encap mode: %v", err)
	}
	for _, encapMode := range encapModes {
		if currentEncapMode == encapMode {
			tb.Skipf("Skipping test for encap mode '%s'", encapMode.String())
		}
	}
}

func ensureAntreaRunning(tb testing.TB, data *TestData) error {
	tb.Logf("Applying Antrea YAML")
	if err := data.deployAntrea(); err != nil {
		return err
	}
	tb.Logf("Waiting for all Antrea DaemonSet Pods")
	if err := data.waitForAntreaDaemonSetPods(defaultTimeout); err != nil {
		return err
	}
	tb.Logf("Checking CoreDNS deployment")
	if err := data.checkCoreDNSPods(defaultTimeout); err != nil {
		return err
	}
	return nil
}

func createDirectory(path string) error {
	return os.Mkdir(path, 0700)
}

func (data *TestData) setupLogDirectoryForTest(testName string) error {
	path := filepath.Join(testOptions.logsExportDir, testName)
	// remove directory if it already exists. This ensures that we start with an empty
	// directory
	_ = os.RemoveAll(path)
	err := createDirectory(path)
	if err != nil {
		return err
	}
	data.logsDirForTestCase = path
	return nil
}

func setupTest(tb testing.TB) (*TestData, error) {
	if err := testData.setupLogDirectoryForTest(tb.Name()); err != nil {
		tb.Errorf("Error creating logs directory '%s': %v", testData.logsDirForTestCase, err)
		return nil, err
	}
	tb.Logf("Creating '%s' K8s Namespace", testNamespace)
	if err := ensureAntreaRunning(tb, testData); err != nil {
		return nil, err
	}
	if err := testData.createTestNamespace(); err != nil {
		return nil, err
	}
	return testData, nil
}

func setupTestWithIPFIXCollector(tb testing.TB) (*TestData, error) {
	data := &TestData{}
	if err := data.setupLogDirectoryForTest(tb.Name()); err != nil {
		tb.Errorf("Error creating logs directory '%s': %v", data.logsDirForTestCase, err)
		return nil, err
	}
	tb.Logf("Creating K8s clientset")
	if err := data.createClient(); err != nil {
		return nil, err
	}
	tb.Logf("Creating '%s' K8s Namespace", testNamespace)
	if err := data.createTestNamespace(); err != nil {
		return nil, err
	}
	// Create pod using ipfix collector image
	if err := data.createPodOnNode("ipfix-collector", masterNodeName(), ipfixCollectorImage, nil, nil, nil, nil, true, nil); err != nil {
		tb.Fatalf("Error when creating the ipfix collector Pod: %v", err)
	}
	ipfixCollectorIP, err := data.podWaitForIP(defaultTimeout, "ipfix-collector", testNamespace)
	if err != nil {
		tb.Fatalf("Error when waiting to get ipfix collector Pod IP: %v", err)
	}
	tb.Logf("Applying Antrea YAML with ipfix collector address")
	if err := data.deployAntreaFlowExporter(ipfixCollectorIP + ":" + ipfixCollectorPort + ":tcp"); err != nil {
		return data, err
	}
	tb.Logf("Checking CoreDNS deployment")
	if err := data.checkCoreDNSPods(defaultTimeout); err != nil {
		return data, err
	}
	return data, nil
}

func exportLogs(tb testing.TB, data *TestData, logsSubDir string, writeNodeLogs bool) {
	if tb.Skipped() {
		return
	}
	// if test was successful and --logs-export-on-success was not provided, we do not export
	// any logs.
	if !tb.Failed() && !testOptions.logsExportOnSuccess {
		return
	}
	const timeFormat = "Jan02-15-04-05"
	timeStamp := time.Now().Format(timeFormat)
	logsDir := filepath.Join(data.logsDirForTestCase, fmt.Sprintf("%s.%s", logsSubDir, timeStamp))
	err := createDirectory(logsDir)
	if err != nil {
		tb.Errorf("Error when creating logs directory '%s': %v", logsDir, err)
		return
	}
	tb.Logf("Exporting test logs to '%s'", logsDir)
	// for now we just retrieve the logs for the Antrea Pods, but maybe we can find a good way to
	// retrieve the logs for the test Pods in the future (before deleting them) if it is useful
	// for debugging.

	// getPodWriter creates the file with name nodeName-podName-suffix. It returns nil if the
	// file cannot be created. File must be closed by the caller.
	getPodWriter := func(nodeName, podName, suffix string) *os.File {
		logFile := filepath.Join(logsDir, fmt.Sprintf("%s-%s-%s", nodeName, podName, suffix))
		f, err := os.Create(logFile)
		if err != nil {
			tb.Errorf("Error when creating log file '%s': '%v'", logFile, err)
			return nil
		}
		return f
	}

	// runKubectl runs the provided kubectl command on the master Node and returns the
	// output. It returns an empty string in case of error.
	runKubectl := func(cmd string) string {
		rc, stdout, _, err := RunCommandOnNode(masterNodeName(), cmd)
		if err != nil || rc != 0 {
			tb.Errorf("Error when running this kubectl command on master Node: %s", cmd)
			return ""
		}
		return stdout
	}

	// dump the logs for Antrea Pods to disk.
	writePodLogs := func(nodeName, podName, nsName string) error {
		w := getPodWriter(nodeName, podName, "logs")
		if w == nil {
			return nil
		}
		defer w.Close()
		cmd := fmt.Sprintf("kubectl -n %s logs --all-containers %s", nsName, podName)
		stdout := runKubectl(cmd)
		if stdout == "" {
			return nil
		}
		w.WriteString(stdout)
		return nil
	}
	data.forAllMatchingPodsInNamespace("app=antrea", antreaNamespace, writePodLogs)

	// dump the logs for monitoring Pods to disk.
	data.forAllMatchingPodsInNamespace("", monitoringNamespace, writePodLogs)

	// dump the output of "kubectl describe" for Antrea pods to disk.
	data.forAllMatchingPodsInNamespace("app=antrea", antreaNamespace, func(nodeName, podName, nsName string) error {
		w := getPodWriter(nodeName, podName, "describe")
		if w == nil {
			return nil
		}
		defer w.Close()
		cmd := fmt.Sprintf("kubectl -n %s describe pod %s", nsName, podName)
		stdout := runKubectl(cmd)
		if stdout == "" {
			return nil
		}
		w.WriteString(stdout)
		return nil
	})

	if !writeNodeLogs {
		return
	}
	// getNodeWriter creates the file with name nodeName-suffix. It returns nil if the file
	// cannot be created. File must be closed by the caller.
	getNodeWriter := func(nodeName, suffix string) *os.File {
		logFile := filepath.Join(logsDir, fmt.Sprintf("%s-%s", nodeName, suffix))
		f, err := os.Create(logFile)
		if err != nil {
			tb.Errorf("Error when creating log file '%s': '%v'", logFile, err)
			return nil
		}
		return f
	}
	// export kubelet logs with journalctl for each Node. If the Nodes do not use journalctl we
	// print a log message. If kubelet is not run with systemd, the log file will be empty.
	if err := forAllNodes(func(nodeName string) error {
		const numLines = 100
		// --no-pager ensures the command does not hang.
		cmd := fmt.Sprintf("journalctl -u kubelet -n %d --no-pager", numLines)
		rc, stdout, _, err := RunCommandOnNode(nodeName, cmd)
		if err != nil || rc != 0 {
			// return an error and skip subsequent Nodes
			return fmt.Errorf("error when running journalctl on Node '%s', is it available?", nodeName)
		}
		w := getNodeWriter(nodeName, "kubelet")
		if w == nil {
			// move on to the next Node
			return nil
		}
		defer w.Close()
		w.WriteString(stdout)
		return nil
	}); err != nil {
		tb.Logf("Error when exporting kubelet logs: %v", err)
	}
}

func teardownTest(tb testing.TB, data *TestData) {
	exportLogs(tb, data, "beforeTeardown", true)
	if empty, _ := IsDirEmpty(data.logsDirForTestCase); empty {
		_ = os.Remove(data.logsDirForTestCase)
	}
	tb.Logf("Deleting '%s' K8s Namespace", testNamespace)
	if err := data.deleteTestNamespace(defaultTimeout); err != nil {
		tb.Logf("Error when tearing down test: %v", err)
	}
}

func deletePodWrapper(tb testing.TB, data *TestData, name string) {
	tb.Logf("Deleting Pod '%s'", name)
	if err := data.deletePod(name); err != nil {
		tb.Logf("Error when deleting Pod: %v", err)
	}
}

// createTestBusyboxPods creates the desired number of busybox Pods and wait for their IP address to
// become available. This is a common patter in our tests, so having this helper function makes
// sense. It calls Fatalf in case of error, so it must be called from the goroutine running the test
// or benchmark function. You can create all the Pods on the same Node by setting nodeName. If
// nodeName is the empty string, each Pod will be created on an arbitrary
// Node. createTestBusyboxPods returns the cleanupFn function which can be used to delete the
// created Pods. Pods are created in parallel to reduce the time required to run the tests.
func createTestBusyboxPods(tb testing.TB, data *TestData, num int, nodeName string) (
	podNames []string, podIPs []string, cleanupFn func(),
) {
	cleanupFn = func() {
		var wg sync.WaitGroup
		for _, podName := range podNames {
			wg.Add(1)
			go func(name string) {
				deletePodWrapper(tb, data, name)
				wg.Done()
			}(podName)
		}
		wg.Wait()
	}

	type podData struct {
		podName string
		podIP   string
		err     error
	}

	createPodAndGetIP := func() (string, string, error) {
		podName := randName("test-pod-")

		tb.Logf("Creating a busybox test Pod '%s' and waiting for IP", podName)
		if err := data.createBusyboxPodOnNode(podName, nodeName); err != nil {
			tb.Errorf("Error when creating busybox test Pod '%s': %v", podName, err)
			return "", "", err
		}

		if podIP, err := data.podWaitForIP(defaultTimeout, podName, testNamespace); err != nil {
			tb.Errorf("Error when waiting for IP for Pod '%s': %v", podName, err)
			return podName, "", err
		} else {
			return podName, podIP, nil
		}
	}

	podsCh := make(chan podData, num)

	for i := 0; i < num; i++ {
		go func() {
			podName, podIP, err := createPodAndGetIP()
			podsCh <- podData{podName, podIP, err}
		}()
	}

	errCnt := 0
	for i := 0; i < num; i++ {
		pod := <-podsCh
		if pod.podName != "" {
			podNames = append(podNames, pod.podName)
			podIPs = append(podIPs, pod.podIP)
		}
		if pod.err != nil {
			errCnt++
		}
	}
	if errCnt > 0 {
		defer cleanupFn()
		tb.Fatalf("%d / %d Pods could not be created successfully", errCnt, num)
	}

	return podNames, podIPs, cleanupFn
}
