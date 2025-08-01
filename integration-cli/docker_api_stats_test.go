package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/request"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

var expectedNetworkInterfaceStats = strings.Split("rx_bytes rx_dropped rx_errors rx_packets tx_bytes tx_dropped tx_errors tx_packets", " ")

func (s *DockerAPISuite) TestAPIStatsNoStreamGetCpu(c *testing.T) {
	skip.If(c, RuntimeIsWindowsContainerd(), "FIXME: Broken on Windows + containerd combination")
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")
	out := cli.DockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "while true;usleep 100; do echo 'Hello'; done").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)
	resp, body, err := request.Get(testutil.GetContext(c), fmt.Sprintf("/containers/%s/stats?stream=false", id))
	assert.NilError(c, err)
	assert.Equal(c, resp.StatusCode, http.StatusOK)
	assert.Equal(c, resp.Header.Get("Content-Type"), "application/json")
	assert.Equal(c, resp.Header.Get("Content-Type"), "application/json")

	var v *container.StatsResponse
	err = json.NewDecoder(body).Decode(&v)
	assert.NilError(c, err)
	body.Close()

	cpuPercent := 0.0

	if testEnv.DaemonInfo.OSType != "windows" {
		cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage)
		systemDelta := float64(v.CPUStats.SystemUsage - v.PreCPUStats.SystemUsage)
		cpuPercent = (cpuDelta / systemDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100.0
	} else {
		// Max number of 100ns intervals between the previous time read and now
		possIntervals := uint64(v.Read.Sub(v.PreRead).Nanoseconds()) // Start with number of ns intervals
		possIntervals /= 100                                         // Convert to number of 100ns intervals
		possIntervals *= uint64(v.NumProcs)                          // Multiple by the number of processors

		// Intervals used
		intervalsUsed := v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage

		// Percentage avoiding divide-by-zero
		if possIntervals > 0 {
			cpuPercent = float64(intervalsUsed) / float64(possIntervals) * 100.0
		}
	}

	assert.Assert(c, cpuPercent != 0.0, "docker stats with no-stream get cpu usage failed: was %v", cpuPercent)
}

func (s *DockerAPISuite) TestAPIStatsStoppedContainerInGoroutines(c *testing.T) {
	out := cli.DockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "echo 1").Stdout()
	id := strings.TrimSpace(out)

	getGoRoutines := func() int {
		_, body, err := request.Get(testutil.GetContext(c), "/info")
		assert.NilError(c, err)
		info := system.Info{}
		err = json.NewDecoder(body).Decode(&info)
		assert.NilError(c, err)
		body.Close()
		return info.NGoroutines
	}

	// When the HTTP connection is closed, the number of goroutines should not increase.
	routines := getGoRoutines()
	_, body, err := request.Get(testutil.GetContext(c), "/containers/"+id+"/stats")
	assert.NilError(c, err)
	body.Close()

	t := time.After(30 * time.Second)
	for {
		select {
		case <-t:
			assert.Assert(c, getGoRoutines() <= routines)
			return
		default:
			if n := getGoRoutines(); n <= routines {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (s *DockerAPISuite) TestAPIStatsNetworkStats(c *testing.T) {
	skip.If(c, RuntimeIsWindowsContainerd(), "FIXME: Broken on Windows + containerd combination")
	testRequires(c, testEnv.IsLocalDaemon)

	id := runSleepingContainer(c)
	cli.WaitRun(c, id)

	// Retrieve the container address
	net := "bridge"
	if testEnv.DaemonInfo.OSType == "windows" {
		net = "nat"
	}
	contIP := findContainerIP(c, id, net)
	numPings := 1

	var preRxPackets uint64
	var preTxPackets uint64
	var postRxPackets uint64
	var postTxPackets uint64

	// Get the container networking stats before and after pinging the container
	nwStatsPre := getNetworkStats(c, id)
	for _, v := range nwStatsPre {
		preRxPackets += v.RxPackets
		preTxPackets += v.TxPackets
	}

	countParam := "-c"
	if runtime.GOOS == "windows" {
		countParam = "-n" // Ping count parameter is -n on Windows
	}
	pingout, err := exec.Command("ping", contIP, countParam, strconv.Itoa(numPings)).CombinedOutput()
	if err != nil && runtime.GOOS == "linux" {
		// If it fails then try a work-around, but just for linux.
		// If this fails too then go back to the old error for reporting.
		//
		// The ping will sometimes fail due to an apparmor issue where it
		// denies access to the libc.so.6 shared library - running it
		// via /lib64/ld-linux-x86-64.so.2 seems to work around it.
		pingout2, err2 := exec.Command("/lib64/ld-linux-x86-64.so.2", "/bin/ping", contIP, "-c", strconv.Itoa(numPings)).CombinedOutput()
		if err2 == nil {
			pingout = pingout2
			err = err2
		}
	}
	assert.NilError(c, err)
	pingouts := string(pingout[:])
	nwStatsPost := getNetworkStats(c, id)
	for _, v := range nwStatsPost {
		postRxPackets += v.RxPackets
		postTxPackets += v.TxPackets
	}

	// Verify the stats contain at least the expected number of packets
	// On Linux, account for ARP.
	expRxPkts := preRxPackets + uint64(numPings)
	expTxPkts := preTxPackets + uint64(numPings)
	if testEnv.DaemonInfo.OSType != "windows" {
		expRxPkts++
		expTxPkts++
	}
	assert.Assert(c, postTxPackets >= expTxPkts, "Reported less TxPackets than expected. Expected >= %d. Found %d. %s", expTxPkts, postTxPackets, pingouts)
	assert.Assert(c, postRxPackets >= expRxPkts, "Reported less RxPackets than expected. Expected >= %d. Found %d. %s", expRxPkts, postRxPackets, pingouts)
}

func (s *DockerAPISuite) TestAPIStatsNetworkStatsVersioning(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	id := runSleepingContainer(c)
	cli.WaitRun(c, id)

	statsJSONBlob := getStats(c, id)
	assert.Assert(c, jsonBlobHasGTE121NetworkStats(statsJSONBlob), "Stats JSON blob from API does not look like a >=v1.21 API stats structure", statsJSONBlob)
}

func getNetworkStats(t *testing.T, id string) map[string]container.NetworkStats {
	var st *container.StatsResponse

	_, body, err := request.Get(testutil.GetContext(t), "/containers/"+id+"/stats?stream=false")
	assert.NilError(t, err)

	err = json.NewDecoder(body).Decode(&st)
	assert.NilError(t, err)
	body.Close()

	return st.Networks
}

// getStats returns stats result for the
// container with id using an API call with version apiVersion. Since the
// stats result type differs between API versions, we simply return
// map[string]interface{}.
func getStats(t *testing.T, id string) map[string]interface{} {
	t.Helper()
	stats := make(map[string]interface{})

	_, body, err := request.Get(testutil.GetContext(t), "/containers/"+id+"/stats?stream=false")
	assert.NilError(t, err)
	defer body.Close()

	err = json.NewDecoder(body).Decode(&stats)
	assert.NilError(t, err, "failed to decode stat: %s", err)

	return stats
}

func jsonBlobHasGTE121NetworkStats(blob map[string]interface{}) bool {
	networksStatsIntfc, ok := blob["networks"]
	if !ok {
		return false
	}
	networksStats, ok := networksStatsIntfc.(map[string]interface{})
	if !ok {
		return false
	}
	for _, networkInterfaceStatsIntfc := range networksStats {
		networkInterfaceStats, ok := networkInterfaceStatsIntfc.(map[string]interface{})
		if !ok {
			return false
		}
		for _, expectedKey := range expectedNetworkInterfaceStats {
			if _, ok := networkInterfaceStats[expectedKey]; !ok {
				return false
			}
		}
	}
	return true
}

func (s *DockerAPISuite) TestAPIStatsContainerNotFound(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	apiClient, err := client.NewClientWithOpts(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	expected := "No such container: nonexistent"

	_, err = apiClient.ContainerStats(testutil.GetContext(c), "nonexistent", true)
	assert.ErrorContains(c, err, expected)
	_, err = apiClient.ContainerStats(testutil.GetContext(c), "nonexistent", false)
	assert.ErrorContains(c, err, expected)
}

func (s *DockerAPISuite) TestAPIStatsNoStreamConnectedContainers(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	id1 := runSleepingContainer(c)
	cli.WaitRun(c, id1)

	id2 := runSleepingContainer(c, "--net", "container:"+id1)
	cli.WaitRun(c, id2)

	ch := make(chan error, 1)
	go func() {
		resp, body, err := request.Get(testutil.GetContext(c), "/containers/"+id2+"/stats?stream=false")
		defer body.Close()
		if err != nil {
			ch <- err
		}
		if resp.StatusCode != http.StatusOK {
			ch <- fmt.Errorf("Invalid StatusCode %v", resp.StatusCode)
		}
		if resp.Header.Get("Content-Type") != "application/json" {
			ch <- fmt.Errorf("Invalid 'Content-Type' %v", resp.Header.Get("Content-Type"))
		}
		var v *container.StatsResponse
		if err := json.NewDecoder(body).Decode(&v); err != nil {
			ch <- err
		}
		ch <- nil
	}()

	select {
	case err := <-ch:
		assert.NilError(c, err, "Error in stats Engine API: %v", err)
	case <-time.After(15 * time.Second):
		c.Fatalf("Stats did not return after timeout")
	}
}
