package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	iopsTablePrefix = "iops-table-"
)

// Plugin groups the methods a plugin needs
type Plugin struct {
	HostID string

	lock sync.Mutex
}

type report struct {
	Host    topology
	Plugins []pluginSpec
}

type topology struct {
	Nodes             map[string]node             `json:"nodes"`
	Controls          map[string]control          `json:"controls"`
	MetadataTemplates map[string]metadataTemplate `json:"metadata_templates,omitempty"`
	TableTemplates    map[string]tableTemplate    `json:"table_templates,omitempty"`
}

type tableTemplate struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Prefix  string    `json:"prefix"`
	Type    string    `json:"type"`
	Columns []columns `json:"columns"`
}

type columns struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"` // Human-readable descriptor for this row
	Datatype string `json:"dataType,omitempty"`
}

type metadataTemplate struct {
	ID       string  `json:"id"`
	Label    string  `json:"label,omitempty"`    // Human-readable descriptor for this row
	Truncate int     `json:"truncate,omitempty"` // If > 0, truncate the value to this length.
	Datatype string  `json:"dataType,omitempty"`
	Priority float64 `json:"priority,omitempty"`
	From     string  `json:"from,omitempty"` // Defines how to get the value from a report node
}

type node struct {
	LatestControls map[string]controlEntry `json:"latestControls,omitempty"`
	Latest         map[string]stringEntry  `json:"latest,omitempty"`
}

type controlEntry struct {
	Timestamp time.Time   `json:"timestamp"`
	Value     controlData `json:"value"`
}

type controlData struct {
	Dead bool `json:"dead"`
}

type control struct {
	ID    string `json:"id"`
	Human string `json:"human"`
	Icon  string `json:"icon"`
	Rank  int    `json:"rank"`
}

type stringEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Value     string    `json:"value"`
}

type pluginSpec struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Interfaces  []string `json:"interfaces"`
	APIVersion  string   `json:"api_version,omitempty"`
}

type iopsData struct {
	Device  string
	Tps     string
	Readps  string
	Writeps string
}

func setupSocket(socketPath string) (net.Listener, error) {
	os.RemoveAll(filepath.Dir(socketPath))
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create directory %q: %v", filepath.Dir(socketPath), err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %q: %v", socketPath, err)
	}

	log.Printf("Listening on: unix://%s", socketPath)
	return listener, nil
}

func setupSignals(socketPath string) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		os.RemoveAll(filepath.Dir(socketPath))
		os.Exit(0)
	}()
}

func main() {
	// We put the socket in a sub-directory to have more control on the permissions
	const socketPath = "/var/run/scope/plugins/iops/iops.sock"
	hostID, _ := os.Hostname()

	// Handle the exit signal
	setupSignals(socketPath)

	log.Printf("Starting on %s...\n", hostID)

	// Check we can get the iops for the system
	_, err := iops()
	if err != nil {
		log.Fatal(err)
	}

	listener, err := setupSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		listener.Close()
		os.RemoveAll(filepath.Dir(socketPath))
	}()

	plugin := &Plugin{HostID: hostID}
	http.HandleFunc("/report", plugin.Report)
	// http.HandleFunc("/control", plugin.Control)
	if err := http.Serve(listener, nil); err != nil {
		log.Printf("error: %v", err)
	}
}

// Report is called by scope when a new report is needed. It is part of the
// "reporter" interface, which all plugins must implement.
func (p *Plugin) Report(w http.ResponseWriter, r *http.Request) {
	p.lock.Lock()
	defer p.lock.Unlock()
	log.Println(r.URL.String())
	rpt, err := p.makeReport()
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, err := json.Marshal(*rpt)
	if err != nil {
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func (p *Plugin) makeReport() (*report, error) {
	rpt := &report{
		Host: topology{
			Nodes: map[string]node{
				p.getTopologyHost(): {
					LatestControls: map[string]controlEntry{},
					Latest:         getLatests(),
				},
			},
			Controls:          map[string]control{},
			MetadataTemplates: getMetadataTemplate(),
			TableTemplates:    getTableTemplate(),
		},
		Plugins: []pluginSpec{
			{
				ID:          "iops",
				Label:       "iops",
				Description: "Adds a IOPS details to Host",
				Interfaces:  []string{"reporter"},
				APIVersion:  "1",
			},
		},
	}
	return rpt, nil
}

func iops() ([]iopsData, error) {
	return iostat()
}

// Get the latest iostat values
func iostat() ([]iopsData, error) {
	out, err := exec.Command("iostat", "-d").Output()
	if err != nil {
		return nil, fmt.Errorf("iops: %v", err)
	}

	// Linux 4.2.0-25-generic (a109563eab38)	04/01/16	_x86_64_(4 CPU)
	//
	// avg-cpu:  %user   %nice %system %iowait  %steal   %idle
	//	          2.37    0.00    1.58    0.01    0.00   96.04
	lines := strings.Split(string(out), "\n")
	if len(lines) < 3 {
		return nil, fmt.Errorf("iops: unexpected output: %q", out)
	}

	iops := make([]iopsData, len(lines)-3)
	var i = 0

	for index, line := range lines {
		if line == "" && index != 1 {
			break
		}
		if index > 2 {
			values := strings.Fields(line)
			iops[i].Device = values[0]
			iops[i].Tps = values[1]
			iops[i].Readps = values[2]
			iops[i].Writeps = values[3]
			i++
		}
	}

	if len(iops) <= 0 {
		return nil, fmt.Errorf("iops: unexpected output: %q", out)
	}
	return iops, nil
}

func (p *Plugin) getTopologyHost() string {
	return fmt.Sprintf("%s;<host>", p.HostID)
}

func getLatests() map[string]stringEntry {
	ts := time.Now()
	IopsData, err := iops()
	if err != nil {
		return nil
	}

	latests := map[string]stringEntry{}

	for index, value := range IopsData {
		latests["iops-table-"+strconv.Itoa(index+1)+"___device"] = stringEntry{
			Timestamp: ts,
			Value:     value.Device,
		}
		latests["iops-table-"+strconv.Itoa(index+1)+"___tps"] = stringEntry{
			Timestamp: ts,
			Value:     value.Tps,
		}
		latests["iops-table-"+strconv.Itoa(index+1)+"___readps"] = stringEntry{
			Timestamp: ts,
			Value:     value.Readps,
		}
		latests["iops-table-"+strconv.Itoa(index+1)+"___writeps"] = stringEntry{
			Timestamp: ts,
			Value:     value.Writeps,
		}
	}

	return latests
}

func getMetadataTemplate() map[string]metadataTemplate {
	return map[string]metadataTemplate{
		"device": {
			ID:       "device",
			Label:    "Device",
			Truncate: 0,
			Datatype: "",
			Priority: 1,
			From:     "latest",
		},
		"tps": {
			ID:       "tps",
			Label:    "tps",
			Truncate: 0,
			Datatype: "",
			Priority: 1,
			From:     "latest",
		},
		"readps": {
			ID:       "readps",
			Label:    "kB_read/s",
			Truncate: 0,
			Datatype: "",
			Priority: 1,
			From:     "latest",
		},
		"writeps": {
			ID:       "writeps",
			Label:    "kB_wrtn/s",
			Truncate: 0,
			Datatype: "",
			Priority: 1,
			From:     "latest",
		},
	}
}

func getTableTemplate() map[string]tableTemplate {
	return map[string]tableTemplate{
		"iops-table-": {
			ID:     "iops-table-",
			Label:  "Iops",
			Prefix: iopsTablePrefix,
			Type:   "multicolumn-table",
			Columns: []columns{
				{
					ID:       "device",
					Label:    "Device",
					Datatype: "",
				},
				{
					ID:       "tps",
					Label:    "tps",
					Datatype: "",
				},
				{
					ID:       "readps",
					Label:    "kB_read/s",
					Datatype: "",
				},
				{
					ID:       "writeps",
					Label:    "kB_wrtn/s",
					Datatype: "",
				},
			},
		},
	}
}
