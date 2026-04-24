package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digineo/go-ping"
)

func strPtr(s string) *string {
	return &s
}

func TestPushToVictoriaMetricsFormatsBodyAndClearsHistory(t *testing.T) {
	t.Parallel()

	ts1 := time.Unix(1710000000, 0)
	ts2 := ts1.Add(3 * time.Second)

	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	device := &Device{
		Host: "example.local",
		Name: strPtr("example"),
		History: []Event{
			{RTT: 1500 * time.Microsecond, Timestamp: ts1, Error: nil},
			{RTT: 0, Timestamp: ts2, Error: io.EOF},
		},
	}

	if err := pushToVictoriaMetrics(server.URL, device); err != nil {
		t.Fatalf("pushToVictoriaMetrics returned error: %v", err)
	}

	if len(device.History) != 0 {
		t.Fatalf("expected history to be cleared, got %d event(s)", len(device.History))
	}

	expected := "" +
		"host_rtt{host=\"example.local\",name=\"example\"} 1500 " +
		"1710000000000\n" +
		"host_up{host=\"example.local\",name=\"example\"} 1 1710000000000\n" +
		"host_up{host=\"example.local\",name=\"example\"} 0 1710000003000\n"

	if gotBody != expected {
		t.Fatalf("unexpected payload\nexpected:\n%s\ngot:\n%s", expected, gotBody)
	}
}

func TestPushToVictoriaMetricsRequeuesOnBadStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("backend error"))
	}))
	defer server.Close()

	ts := time.Unix(1710000000, 0)
	device := &Device{
		Host: "example.local",
		Name: strPtr("example"),
		History: []Event{
			{RTT: 2 * time.Millisecond, Timestamp: ts, Error: nil},
		},
	}

	err := pushToVictoriaMetrics(server.URL, device)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "unexpected status code: 500") {
		t.Fatalf("expected 500 error, got: %v", err)
	}

	if len(device.History) != 1 {
		t.Fatalf("expected history to be re-queued, got %d event(s)", len(device.History))
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	content := "" +
		"push_url: http://127.0.0.1:8428/api/v1/import/prometheus\n" +
		"devices:\n" +
		"  - host: localhost\n"

	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	defer func() {
		if chdirErr := os.Chdir(origWD); chdirErr != nil {
			t.Fatalf("restore working directory: %v", chdirErr)
		}
	}()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir temp directory: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}

	if loaded.Timeout == nil || *loaded.Timeout != time.Second {
		t.Fatalf("expected default timeout of 1s, got %v", loaded.Timeout)
	}
	if loaded.Interval == nil || *loaded.Interval != time.Second {
		t.Fatalf("expected default interval of 1s, got %v", loaded.Interval)
	}
	if loaded.PayloadSize == nil || *loaded.PayloadSize != 32 {
		t.Fatalf("expected default payload size of 32, got %v", loaded.PayloadSize)
	}
	if len(loaded.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(loaded.Devices))
	}
	if loaded.Devices[0].Name == nil || *loaded.Devices[0].Name != "localhost" {
		t.Fatalf("expected default device name to be host, got %v", loaded.Devices[0].Name)
	}
	if loaded.Devices[0].Remote == nil {
		t.Fatalf("expected resolved remote address")
	}
}

func TestGetConfigSnapshotCopiesDeviceSlice(t *testing.T) {
	t.Parallel()

	interval := 5 * time.Second
	payload := 64
	hostName := "one"
	originalCfg := &Config{
		PushURL:     "http://vm.local/import",
		Interval:    &interval,
		PayloadSize: &payload,
		Devices: []*Device{
			{Host: "1.1.1.1", Name: &hostName},
		},
	}

	cfg_mtx.Lock()
	prevCfg := cfg
	cfg = originalCfg
	cfg_mtx.Unlock()
	t.Cleanup(func() {
		cfg_mtx.Lock()
		cfg = prevCfg
		cfg_mtx.Unlock()
	})

	snap := getConfigSnapshot()
	if len(snap.Devices) != 1 {
		t.Fatalf("expected 1 device in snapshot, got %d", len(snap.Devices))
	}

	newName := "changed"
	snap.Devices = append(snap.Devices, &Device{Host: "2.2.2.2", Name: &newName})
	snap.Devices[0] = &Device{Host: "9.9.9.9", Name: &newName}

	if len(cfg.Devices) != 1 {
		t.Fatalf("expected cfg devices length to remain 1, got %d", len(cfg.Devices))
	}
	if cfg.Devices[0].Host != "1.1.1.1" {
		t.Fatalf("expected original first device to remain unchanged, got %s", cfg.Devices[0].Host)
	}
}

func TestDevicePingLocalhost(t *testing.T) {
	timeout := 2 * time.Second
	name := "localhost"
	remote := &net.IPAddr{IP: net.ParseIP("127.0.0.1")}
	if remote.IP == nil {
		t.Fatal("failed to parse localhost IP")
	}

	localPinger, err := ping.New("0.0.0.0", "::")
	if err != nil {
		t.Skipf("skipping true ping test: cannot initialize pinger in this environment: %v", err)
	}
	defer localPinger.Close()

	prevPinger := pinger
	pinger = localPinger
	t.Cleanup(func() {
		pinger = prevPinger
	})

	device := &Device{
		Host:    "localhost",
		Name:    &name,
		Timeout: &timeout,
		Remote:  remote,
	}

	rtt, err := device.ping()
	if err != nil {
		t.Skipf("skipping true ping test: ping to localhost is not permitted here: %v", err)
	}

	if rtt < 0 {
		t.Fatalf("expected non-negative RTT, got %v", rtt)
	}
	if len(device.History) != 1 {
		t.Fatalf("expected one history event, got %d", len(device.History))
	}
	if device.History[0].Error != nil {
		t.Fatalf("expected successful ping event, got error: %v", device.History[0].Error)
	}
}
