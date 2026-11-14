// 职责: 生成并上报 commandcode CLI 的机器指纹（/alpha/fingerprint/record）。
// 关联文件:
//
//	llm/transformer/commandcode/outbound.go
//	llm/transformer/commandcode/sysinfo.go
//
// 注意事项:
//
//	哈希算法必须与 CLI 一致：sha256("command-code:device-fingerprint:v1" + "\0" + lower(trim(value)))；
//	CLI 每个进程只上报一次，本实现同样以 sync.Once 约束。
package commandcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	fingerprintSalt       = "command-code:device-fingerprint:v1"
	fingerprintRecordPath = "/alpha/fingerprint/record"
	fingerprintVersion    = 1
	fingerprintTimeout    = 10 * time.Second
)

var (
	machineUUIDPattern = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)
	gitEmailPattern    = regexp.MustCompile(`(?im)^\s*email\s*=\s*(.+?)\s*$`)
	cgroupPattern      = regexp.MustCompile(`docker|kubepods|containerd|lxc`)
	cpuModelPattern    = regexp.MustCompile(`(?m)^model name\s*:\s*(.+?)\s*$`)
	memTotalPattern    = regexp.MustCompile(`(?m)^MemTotal:\s*(\d+)\s*kB`)
)

type fingerprintComponents struct {
	MachineIDHash    string   `json:"machineIdHash,omitempty"`
	MACHashes        []string `json:"macHashes,omitempty"`
	OSUserHash       string   `json:"osUserHash,omitempty"`
	HostnameHash     string   `json:"hostnameHash,omitempty"`
	GitEmailHash     string   `json:"gitEmailHash,omitempty"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone,omitempty"`
	Runtime          string   `json:"runtime"`
	CollectorVersion int      `json:"collectorVersion"`
}

type machineSignals struct {
	machineID    string
	macAddresses []string
	osUser       string
	hostname     string
	gitEmail     string
	osRelease    string
	cpuModel     string
	totalMem     uint64
	isContainer  bool
	timezone     string
}

// hashSignal mirrors the CLI: sha256(salt + "\0" + lower(trim(value))) or "".
func hashSignal(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fingerprintSalt + "\x00" + strings.ToLower(value)))
	return hex.EncodeToString(sum[:])
}

func readMachineID(ctx context.Context) string {
	switch runtime.GOOS {
	case "darwin":
		out := shellOutput(ctx, "", "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if m := machineUUIDPattern.FindStringSubmatch(out); m != nil {
			return m[1]
		}
	case "linux":
		for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if data, err := os.ReadFile(path); err == nil {
				if id := strings.TrimSpace(string(data)); id != "" {
					return id
				}
			}
		}
	case "windows":
		out := shellOutput(ctx, "", "reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid")
		if idx := strings.Index(out, "MachineGuid"); idx >= 0 {
			fields := strings.Fields(out[idx:])
			if len(fields) > 0 {
				return fields[len(fields)-1]
			}
		}
	}
	return ""
}

func readMACAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		mac := strings.ToLower(iface.HardwareAddr.String())
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		seen[mac] = struct{}{}
	}
	macs := make([]string, 0, len(seen))
	for mac := range seen {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs
}

func readGitEmail(ctx context.Context) string {
	if email := shellOutput(ctx, "", "git", "config", "--global", "user.email"); email != "" {
		return email
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(home + "/.gitconfig")
	if err != nil {
		return ""
	}
	if m := gitEmailPattern.FindSubmatch(data); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func detectContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil && cgroupPattern.Match(data) {
		return true
	}
	return false
}

func readTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "zoneinfo/"); idx >= 0 {
			return target[idx+len("zoneinfo/"):]
		}
	}
	return ""
}

func readCPUModel(ctx context.Context) string {
	switch runtime.GOOS {
	case "darwin":
		return shellOutput(ctx, "", "sysctl", "-n", "machdep.cpu.brand_string")
	case "linux":
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			if m := cpuModelPattern.FindSubmatch(data); m != nil {
				return string(m[1])
			}
		}
	}
	return ""
}

func readOSRelease(ctx context.Context) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return shellOutput(ctx, "", "uname", "-r")
}

func readTotalMemBytes(ctx context.Context) uint64 {
	switch runtime.GOOS {
	case "darwin":
		out := shellOutput(ctx, "", "sysctl", "-n", "hw.memsize")
		var total uint64
		_, _ = fmt.Sscanf(out, "%d", &total)
		return total
	case "linux":
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			if m := memTotalPattern.FindSubmatch(data); m != nil {
				var kb uint64
				_, _ = fmt.Sscanf(string(m[1]), "%d", &kb)
				return kb * 1024
			}
		}
	}
	return 0
}

func gatherMachineSignals(ctx context.Context) machineSignals {
	osUser := os.Getenv("USER")
	if osUser == "" {
		osUser = os.Getenv("USERNAME")
	}
	if osUser == "" {
		if current, err := user.Current(); err == nil {
			osUser = current.Username
		}
	}
	hostname, _ := os.Hostname()
	return machineSignals{
		machineID:    readMachineID(ctx),
		macAddresses: readMACAddresses(),
		osUser:       osUser,
		hostname:     hostname,
		gitEmail:     readGitEmail(ctx),
		osRelease:    readOSRelease(ctx),
		cpuModel:     readCPUModel(ctx),
		totalMem:     readTotalMemBytes(ctx),
		isContainer:  detectContainer(),
		timezone:     readTimezone(),
	}
}

// buildMachineFingerprint mirrors the CLI's buildMachineFingerprint.
func buildMachineFingerprint(s machineSignals) (string, fingerprintComponents) {
	parts := []string{
		s.machineID,
		strings.Join(s.macAddresses, ","),
	}
	if s.machineID == "" {
		parts = append(parts, s.hostname, s.cpuModel)
	}
	var joined []string
	for _, part := range parts {
		if part != "" {
			joined = append(joined, part)
		}
	}
	thumbInput := strings.Join(joined, "|")
	if thumbInput == "" {
		thumbInput = "unknown"
	}
	thumbSum := sha256.Sum256([]byte(fingerprintSalt + "\x00machine\x00" + thumbInput))

	macHashes := make([]string, 0, len(s.macAddresses))
	for _, mac := range s.macAddresses {
		if hash := hashSignal(mac); hash != "" {
			macHashes = append(macHashes, hash)
		}
	}

	return hex.EncodeToString(thumbSum[:]), fingerprintComponents{
		MachineIDHash:    hashSignal(s.machineID),
		MACHashes:        macHashes,
		OSUserHash:       hashSignal(s.osUser),
		HostnameHash:     hashSignal(s.hostname),
		GitEmailHash:     hashSignal(s.gitEmail),
		Platform:         nodePlatform(),
		Arch:             nodeArch(),
		OSRelease:        s.osRelease,
		CPUModel:         s.cpuModel,
		CPUCount:         runtime.NumCPU(),
		MemGiB:           int(s.totalMem / (1 << 30)),
		IsContainer:      s.isContainer,
		Timezone:         s.timezone,
		Runtime:          "cli",
		CollectorVersion: fingerprintVersion,
	}
}

// scheduleFingerprint fires the CLI's once-per-process fingerprint record in
// the background, detached from the in-flight request's cancellation.
func (t *OutboundTransformer) scheduleFingerprint(ctx context.Context) {
	if t.config.HTTPClient == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fingerprintTimeout)
		defer cancel()
		t.recordFingerprint(ctx)
	}()
}

// recordFingerprint mirrors the CLI's recordCliFingerprint: POST the hashed
// machine signals to /alpha/fingerprint/record with the CLI header set.
func (t *OutboundTransformer) recordFingerprint(ctx context.Context) {
	thumbmark, components := buildMachineFingerprint(gatherMachineSignals(ctx))
	body, err := json.Marshal(map[string]any{
		"thumbmark":  thumbmark,
		"components": components,
	})
	if err != nil {
		return
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "*")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("User-Agent", commandCodeUserAgent)
	headers.Set("Authorization", "Bearer "+t.config.APIKeyProvider.Get(ctx))
	headers.Set("X-Command-Code-Version", Version)
	headers.Set("X-Cli-Environment", "production")
	_, _ = t.config.HTTPClient.Do(ctx, &httpclient.Request{
		Method:      http.MethodPost,
		URL:         strings.TrimRight(t.config.BaseURL, "/") + fingerprintRecordPath,
		Path:        fingerprintRecordPath,
		Headers:     headers,
		ContentType: "application/json",
		Body:        body,
	})
}
