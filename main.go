package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digineo/go-ping"
	"gopkg.in/yaml.v3"
)

type Event struct {
	RTT       time.Duration
	Timestamp time.Time
	Error     error
}

type Device struct {
	Host    string         `yaml:"host"`
	Name    *string        `yaml:"name"`
	Timeout *time.Duration `yaml:"timeout"`
	Remote  *net.IPAddr
	History []Event
	mtx     sync.RWMutex
}

type Config struct {
	PushURL     string         `yaml:"push_url"`
	Interval    *time.Duration `yaml:"interval"`
	Timeout     *time.Duration `yaml:"timeout"`
	BindAddr4   *string        `yaml:"bind_addr4"`
	BindAddr6   *string        `yaml:"bind_addr6"`
	PayloadSize *int           `yaml:"payload_size"`
	Devices     []*Device      `yaml:"devices"`
}

type ConfigSnapshot struct {
	PushURL     string
	Interval    time.Duration
	PayloadSize int
	Devices     []*Device
}

var (
	cfg          *Config
	cfg_mtx      sync.RWMutex
	pinger       *ping.Pinger
	httpClient   = &http.Client{Timeout: 10 * time.Second}
	reloadPeriod = 30 * time.Second
)

func pushToVictoriaMetrics(url string, device *Device) error {
	var buf bytes.Buffer

	device.mtx.Lock()
	if len(device.History) == 0 {
		device.mtx.Unlock()
		return nil
	}

	toPush := append([]Event(nil), device.History...)
	device.History = device.History[:0]
	device.mtx.Unlock()

	for _, e := range toPush {
		// Format: <metric_name>{<labels>} <value> <timestamp_ms>
		// Note: VictoriaMetrics requires Unix Milliseconds for the timestamp
		var upline string
		if e.Error == nil {
			rttline := fmt.Sprintf("host_rtt{host=%q,name=%q} %d %d\n",
				device.Host, *device.Name, e.RTT.Microseconds(), e.Timestamp.UnixMilli())
			buf.WriteString(rttline)
			upline = fmt.Sprintf("host_up{host=%q,name=%q} 1 %d\n",
				device.Host, *device.Name, e.Timestamp.UnixMilli())
		} else {
			upline = fmt.Sprintf("host_up{host=%q,name=%q} 0 %d\n",
				device.Host, *device.Name, e.Timestamp.UnixMilli())
		}

		buf.WriteString(upline)
	}

	// VictoriaMetrics /api/v1/import/prometheus endpoint
	// accepts this text format via POST
	resp, err := httpClient.Post(url, "text/plain", &buf)
	if err != nil {
		device.mtx.Lock()
		device.History = append(toPush, device.History...)
		device.mtx.Unlock()
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		device.mtx.Lock()
		device.History = append(toPush, device.History...)
		device.mtx.Unlock()
		if len(body) > 0 {
			return fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func main() {
	fmt.Println("Starting Reachistorian Collector")

	initialCfg, err := loadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	cfg = initialCfg

	pinger, err = ping.New(*cfg.BindAddr4, *cfg.BindAddr6)
	if err != nil {
		log.Fatalf("failed to create pinger: %v", err)
	}
	pinger.SetPayloadSize(uint16(*cfg.PayloadSize))
	defer pinger.Close()

	go work()
	reloadTicker := time.NewTicker(reloadPeriod)
	defer reloadTicker.Stop()

	for {
		snap := getConfigSnapshot()
		for _, device := range snap.Devices {
			if err := pushToVictoriaMetrics(snap.PushURL, device); err != nil {
				log.Printf("error pushing to VictoriaMetrics for host %s: %v", device.Host, err)
			}
		}

		select {
		case <-reloadTicker.C:
			newCfg, err := loadConfig()
			if err != nil {
				log.Printf("config reload failed, keeping previous config: %v", err)
				continue
			}

			cfg_mtx.Lock()
			cfg = newCfg
			cfg_mtx.Unlock()

			log.Printf("configuration reloaded: %d devices", len(newCfg.Devices))
		default:
			time.Sleep(snap.Interval)
		}
	}
}

func loadConfig() (*Config, error) {
	f, err := os.Open("config.yaml")
	if err != nil {
		return nil, fmt.Errorf("open config.yaml: %w", err)
	}
	defer f.Close()

	decoder := yaml.NewDecoder(f)
	var newCfg Config
	if err := decoder.Decode(&newCfg); err != nil {
		return nil, fmt.Errorf("decode config.yaml: %w", err)
	}

	if strings.TrimSpace(newCfg.PushURL) == "" {
		return nil, errors.New("push_url must be configured")
	}

	if len(newCfg.Devices) == 0 {
		return nil, errors.New("no devices configured")
	}

	if newCfg.Timeout == nil {
		timeout := 1 * time.Second
		newCfg.Timeout = &timeout
	}
	if newCfg.Interval == nil {
		interval := 1 * time.Second
		newCfg.Interval = &interval
	}
	if newCfg.BindAddr4 == nil {
		bindAddr4 := "0.0.0.0"
		newCfg.BindAddr4 = &bindAddr4
	}
	if newCfg.BindAddr6 == nil {
		bindAddr6 := "::"
		newCfg.BindAddr6 = &bindAddr6
	}
	if newCfg.PayloadSize == nil {
		payloadSize := 32
		newCfg.PayloadSize = &payloadSize
	}

	for _, device := range newCfg.Devices {
		if strings.TrimSpace(device.Host) == "" {
			return nil, errors.New("device host must not be empty")
		}
		if device.Timeout == nil {
			device.Timeout = newCfg.Timeout
		}
		if device.Name == nil {
			name := device.Host
			device.Name = &name
		}
		ips, err := resolve(device.Host, *device.Timeout)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve host %s: %w", device.Host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no IP addresses found for host %s", device.Host)
		}
		for _, remote := range ips {
			if v4 := remote.IP.To4() != nil; v4 && *newCfg.BindAddr4 == "" || !v4 && *newCfg.BindAddr6 == "" {
				continue
			}

			device.Remote = &remote
			break
		}
		if device.Remote == nil {
			return nil, fmt.Errorf("no usable IP addresses found for host %s with current bind settings", device.Host)
		}
		log.Printf("resolved host %s to IP %s", device.Host, device.Remote.String())
	}

	return &newCfg, nil
}

func resolve(addr string, timeout time.Duration) ([]net.IPAddr, error) {
	if strings.ContainsRune(addr, '%') {
		ipaddr, err := net.ResolveIPAddr("ip", addr)
		if err != nil {
			return nil, err
		}
		return []net.IPAddr{*ipaddr}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return net.DefaultResolver.LookupIPAddr(ctx, addr)
}

func (d *Device) addEvent(event Event) {
	d.mtx.Lock()
	defer d.mtx.Unlock()
	d.History = append(d.History, event)
}

func (d *Device) ping() (time.Duration, error) {
	rtt, err := pinger.Ping(d.Remote, *d.Timeout)
	d.addEvent(Event{
		RTT:       rtt,
		Timestamp: time.Now(),
		Error:     err,
	})
	return rtt, err
}

func work() {
	for {
		snap := getConfigSnapshot()
		for _, device := range snap.Devices {
			go func(d *Device) {
				_, _ = d.ping()
			}(device)
		}
		time.Sleep(snap.Interval)
	}
}

func getConfigSnapshot() ConfigSnapshot {
	cfg_mtx.RLock()
	defer cfg_mtx.RUnlock()

	devices := make([]*Device, len(cfg.Devices))
	copy(devices, cfg.Devices)

	return ConfigSnapshot{
		PushURL:     cfg.PushURL,
		Interval:    *cfg.Interval,
		PayloadSize: *cfg.PayloadSize,
		Devices:     devices,
	}
}
