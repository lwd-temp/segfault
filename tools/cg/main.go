package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
)

// set during compilation using ldflags
var Version string
var Buildtime string

// CLI flags
var (
	strainFlag     = flag.Float64("strain", 20, "maximum amount of strain per CPU core")
	resultFlag     = flag.String("result", "/sf/config/db/cg", "path where action results are stored")
	timerFlag      = flag.Int("timer", 5, "every how often to check for system load in seconds")
	cgroupPathFlag = flag.String("cgroup", "/sys/fs/cgroup/sf.slice/sf-guest.slice/docker-%s.scope/cgroup.procs", "path of your cgroup.procs file")
	debugFlag      = flag.Bool("debug", false, "activate debug mode")
)

func main() {
	flag.Parse()
	if *debugFlag {
		log.SetLevel(log.DebugLevel)
		log.SetReportCaller(true)
	}

	log.SetFormatter(&log.TextFormatter{
		ForceColors: true,
	})

	// number of virtual cores
	var numCPU = runtime.NumCPU()
	// MAX_LOAD defines the maximum amount of `strain` each CPU can have
	// before triggering our cleanup tasks.
	var MAX_LOAD = *strainFlag * float64(numCPU)
	// last recorded loadavg after a trigger event
	var LAST_LOAD float64 // default value 0.0

	hostname, _ := os.Hostname()
	log.Infof("started protecting [%v] (%v load)", hostname, MAX_LOAD)
	log.Infof("compiled on %v from commit %v", Buildtime, Version)

	for range time.Tick(time.Second * time.Duration(*timerFlag)) {
		CURRENT_LOAD := sysLoad1mAvg()

		if CURRENT_LOAD <= MAX_LOAD {
			continue
		}

		// if load is going down don't trigger
		if CURRENT_LOAD < LAST_LOAD {
			LAST_LOAD = CURRENT_LOAD
			continue
		}

		log.Warnf("[TRIGGER] load (%.2f) on cpu (%v) higher than max_load (%v)", CURRENT_LOAD, numCPU, MAX_LOAD)

		// docker client
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			panic(err)
		}
		log.Debugf("connected to docker client v%v", cli.ClientVersion())

		err = stopContainersBasedOnUsage(cli)
		if err != nil {
			log.Error(err)
		}

	}
}

// stopContainersBasedOnUsage iterates through all the containers on the system
// to find abusive ones and stops them, but only if their name starts w/ lg-*
func stopContainersBasedOnUsage(cli *client.Client) error {
	const filterPrefix = "/lg-*"
	opts := types.ContainerListOptions{}
	opts.All = false // list only running containers
	opts.Filters = filters.NewArgs()
	opts.Filters.Add("name", filterPrefix)

	ctx := context.Background()
	list, err := cli.ContainerList(ctx, opts)
	if err != nil {
		return err
	}

	// mu protects `highestUsage`
	var mu sync.Mutex
	var highestUsage float64

	// used to synchronize goroutines
	var wg = &sync.WaitGroup{}

	// check all containers usage and keep largest value in `highestUsage` var
	for _, c := range list {
		wg.Add(1)
		go func(c types.Container) {
			defer wg.Done()

			usage := containerUsage(cli, c.ID)
			if usage > highestUsage {
				mu.Lock()
				highestUsage = usage
				mu.Unlock()
			}

			log.Infof("[%v] usage (%.2f%%)", c.Names[0][1:], usage)
		}(c)
	}
	wg.Wait()
	log.Infof("[HIGHEST USAGE] %.2f%%", highestUsage)

	for _, c := range list {
		usage := containerUsage(cli, c.ID)
		log.Debugf("allowed to kill %v with usage %v", c.Names[0], usage)

		intPtr := func(v int) *int { return &v }
		var killTimeout = container.StopOptions{
			Signal:  "SIGTERM",
			Timeout: intPtr(5),
		}

		var killThreshold = highestUsage * 0.8 // 80% of highestUsage

		const actionMessage = "STOP (5s) || KILL"
		const abuseMessage = "Your server was shut down because it consumed too many resources. If you feel that this was a mistake then please contact us 💙"

		// stop all containers where usage > `highestUsage` * 0.8
		if usage > killThreshold {
			if killThreshold < 10.0 {
				return fmt.Errorf("StopContainer: operation aborted: threshold too low %.5f", killThreshold)
			}
			log.Warnf("[%v] usage (%.2f%%) > threshold (%.2f%%) | action %v", c.Names[0][1:], usage, killThreshold, actionMessage)

			// message user that he's being abusive
			err = sendMessage(c.ID, abuseMessage)
			if err != nil {
				log.Errorf("unable to message container: %v", err)
			}

			err = printProcs(c.ID, c.Names[0])
			if err != nil {
				log.Error(err)
			}

			ctx := context.Background()
			err = cli.ContainerStop(ctx, c.ID, killTimeout)
			if err != nil {
				log.Error(err)
				continue
			}
		}

		// log stopped containers to disk
		logData := LogData{
			name:      c.Names[0],
			usage:     usage,
			threshold: killThreshold,
			load:      sysLoad1mAvg(),
			action:    actionMessage,
		}
		if err := logData.save(*resultFlag); err != nil {
			log.Error(err)
			continue
		}
	}

	return nil
}

// containerUsage calculates the CPU usage of a container.
func containerUsage(cli *client.Client, cID string) float64 {
	ctx := context.Background()
	stats, err := cli.ContainerStats(ctx, cID, false)
	if err != nil {
		log.Error(err)
		return 0
	}
	defer stats.Body.Close()

	var result ContainerStatsData
	err = json.NewDecoder(stats.Body).Decode(&result)
	if err != nil {
		log.Error(err)
		b, _ := ioutil.ReadAll(stats.Body)
		log.Error(b)
	}

	// https://github.com/docker/cli/blob/53f8ed4bec07084db4208f55987a2ea94b7f01d6/cli/command/container/stats_helpers.go#L166
	// calculations
	cpu_delta := float64(result.CPUStats.CPUUsage.TotalUsage) - float64(result.PrecpuStats.CPUUsage.TotalUsage)
	system_cpu_delta := result.CPUStats.SystemCPUUsage - result.PrecpuStats.SystemCPUUsage
	number_cpus := result.CPUStats.OnlineCpus
	usage := (float64(cpu_delta) / float64(system_cpu_delta)) * float64(number_cpus) * 100.0

	return usage
}

// sendMessage delivers a message to a user's shell.
func sendMessage(cID string, message string) error {
	pidPath := fmt.Sprintf("/var/run/containerd/io.containerd.runtime.v2.task/moby/%v/init.pid", cID)

	pid, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("readfile: %v", err)
	}

	// keeps track of how many FDs we've walked past.
	var fdCount int
	_path := fmt.Sprintf("/proc/%s/root/dev/pts/", pid)
	err = filepath.WalkDir(_path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fdCount++

		_, err = strconv.Atoi(d.Name())
		if err != nil {
			log.Debugf("not a number: %v", d.Name())
			return nil
		}

		log.Debugf("MESSAGE to: %v", path)
		err = _sendMessage(path, message)
		if err != nil {
			return err
		}

		if fdCount > 100 {
			return fmt.Errorf("%v has over 100 file descriptors, probably an attack...", _path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walkpath: %v", err)
	}

	return nil
}

// _sendMessage writes bytes to a file descriptor after doing
// some security checks to make sure it's really a FD.
func _sendMessage(fd, message string) error {

	file, err := os.OpenFile(fd, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	// thank you @nobody for the tips
	if info.Mode().Type() == os.ModeSymlink {
		return fmt.Errorf("%v is a symlink! dodging attack...", file.Name())
	}

	// removed this check as pts/0 is not detected as socket and the message won't be sent.
	// if info.Mode().Type() != os.ModeSocket {
	// 	return fmt.Errorf("%v is NOT a socket! dodging attack...", file.Name())
	// }

	if !term.IsTerminal(int(file.Fd())) {
		return fmt.Errorf("unable to write to %v: not a tty", file.Name())
	}

	_, err = file.Write([]byte(message + "\n"))
	if err != nil {
		return err
	}

	return nil
}

type LogData struct {
	name      string
	usage     float64
	threshold float64
	load      float64
	action    string
}

// run mkdir only once
var mkdirOnce = sync.Once{}

func (a LogData) save(path string) error {

	var err error
	mkdirOnce.Do(func() {
		err = os.MkdirAll(path, 0770)
	})
	if err != nil {
		return err
	}

	t := time.Now().UTC().Unix()
	// example: 1666389757 usage=95.71 threshold=28.61 load=200.23 action=SIGKILL
	data := fmt.Sprintf("%v usage=%.2f threshold=%.2f load=%.2f action=%s ", t, a.usage, a.threshold, a.load, a.action)
	filePath := filepath.Join(path, a.name+".txt")

	if err := os.WriteFile(filePath, []byte(data), 0660); err != nil {
		return err
	}

	log.Debugf("[LOG FILE] %v", filePath)

	return nil
}

func sanitize(s string) string {
	clean := func(s []byte) string {
		j := 0
		for i, b := range s {
			if b == '\x00' {
				s[i] = '\x20'
			}
			if ('a' <= b && b <= 'z') ||
				('A' <= b && b <= 'Z') ||
				('0' <= b && b <= '9') ||
				b == '#' || b == ' ' || b == '-' {
				s[j] = b
				j++
			}
		}
		return string(s[:j])
	}
	s = clean([]byte(s))
	return s
}

func printProcs(cid, cname string) error {
	cname = cname[1:]

	cgroupProcs := fmt.Sprintf(*cgroupPathFlag, cid)
	file, err := os.Open(cgroupProcs)
	if err != nil {
		return err
	}
	var count int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if count > 12 {
			return fmt.Errorf("reached max commands, skipping extra output")
		}
		count++

		cmdline := fmt.Sprintf("/proc/%s/cmdline", scanner.Text())

		// limit each command string to max 255 characters
		if len(cmdline) > 255 {
			cmdline = cmdline[:255]
		}

		file, err := os.Open(cmdline)
		if err != nil {
			return err
		}

		_scanner := bufio.NewScanner(file)
		for _scanner.Scan() {
			data := sanitize(_scanner.Text())
			log.Warnf("[%s] proc: %v", cname, data)
		}
	}
	return nil
}

type ContainerStatsData struct {
	CPUStats struct {
		CPUUsage struct {
			UsageInUsermode   int `json:"usage_in_usermode"`
			TotalUsage        int `json:"total_usage"`
			UsageInKernelmode int `json:"usage_in_kernelmode"`
		} `json:"cpu_usage"`
		SystemCPUUsage int `json:"system_cpu_usage"`
		OnlineCpus     int `json:"online_cpus"`
		ThrottlingData struct {
			Periods          int `json:"periods"`
			ThrottledPeriods int `json:"throttled_periods"`
			ThrottledTime    int `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"cpu_stats"`
	PrecpuStats struct {
		CPUUsage struct {
			PercpuUsage       []int `json:"percpu_usage"`
			UsageInUsermode   int   `json:"usage_in_usermode"`
			TotalUsage        int   `json:"total_usage"`
			UsageInKernelmode int   `json:"usage_in_kernelmode"`
		} `json:"cpu_usage"`
		SystemCPUUsage int `json:"system_cpu_usage"`
		OnlineCpus     int `json:"online_cpus"`
		ThrottlingData struct {
			Periods          int `json:"periods"`
			ThrottledPeriods int `json:"throttled_periods"`
			ThrottledTime    int `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"precpu_stats"`
}
