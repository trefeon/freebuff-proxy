// Package login holds the pure helpers behind the headless OAuth login flow:
// the device fingerprint (mirroring reference/freebuff cli/src/utils/
// fingerprint.ts), the GitHub protocol HTML form parsers and the RFC 6238 TOTP
// code generator. The transport-coupled Client methods (StartCLILogin,
// PollCLILogin, ProtocolGitHubLogin) stay in the upstream wire package, which
// imports this one for the pure helpers.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// The login fingerprint mirrors the official CLI's enhanced device
// fingerprint (reference/freebuff cli/src/utils/fingerprint.ts:88-128):
// sha256 over a deterministic machine JSON, base64url-encoded, prefixed
// "enhanced-" — 43 chars total suffix. It is INTENTIONALLY stable across
// restarts (anonymous-id.ts:20-24: anti-abuse determinism), so the id is
// computed once per process. The old crypto/rand mint was wrong on both
// axes: random per login and a hex alphabet.

// fingerprintOnce caches the process-wide login fingerprint id.
var (
	fingerprintOnce sync.Once
	fingerprintID   string
)

// GenerateFingerprintID returns the stable machine-derived "enhanced-"
// fingerprint id sent as fingerprintId in POST /api/auth/cli/code.
func GenerateFingerprintID() string {
	fingerprintOnce.Do(func() {
		hostname, _ := os.Hostname()
		macs, ifaceCount := networkIdentity()
		fingerprintID = fingerprintIDFrom(hostname, macs, ifaceCount, runtime.NumCPU())
	})
	return fingerprintID
}

// GenerateIsolatedFingerprintID creates a fresh, random "enhanced-" fingerprint
// (mirroring gen-freebuff-token.sh: "enhanced-" + base64url(random-32-bytes)).
// Used by the dashboard login wizard and multi-account flows so multiple
// accounts added to a pool are not correlated by a shared hardware identifier.
func GenerateIsolatedFingerprintID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fingerprintIDFrom(time.Now().Format(time.RFC3339Nano), nil, 0, runtime.NumCPU())
	}
	return "enhanced-" + base64.RawURLEncoding.EncodeToString(b[:])
}

// networkIdentity collects the host's non-loopback MAC addresses (sorted,
// zero MACs dropped) and the TOTAL interface count — the official shape
// counts every entry of node's networkInterfaces() map, loopbacks included
// (fingerprint.ts:100-113).
func networkIdentity() (macs []string, interfaceCount int) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, 0
	}
	for _, ifc := range ifaces {
		interfaceCount++
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		mac := ifc.HardwareAddr.String()
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs, interfaceCount
}

// nodePlatform/nodeArch map Go identifiers to the Node spellings the
// official fingerprint JSON uses (process.platform / process.arch).
func nodePlatform(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos
}

func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return goarch
	}
}

// fingerprintIDFrom builds the official key structure over deterministic
// host-derived values and hashes it exactly like calculateEnhancedFingerprint:
// JSON.stringify → sha256 → base64url → "enhanced-"+hash (43 chars). The
// struct field order MUST match fingerprint.ts's object literal order —
// JSON bytes are hashed verbatim. Fields Go cannot read portably
// (SMBIOS system strings, Node version, login shell) carry fixed realistic
// literals or digests of the same machine seed; they are pinned persona
// values and are NEVER transmitted upstream — only the final hash leaves
// this process — so realism only needs to survive code review.
func fingerprintIDFrom(hostname string, macs []string, ifaceCount, cpuCount int) string {
	machineSeed := hostname + "|" + strings.Join(macs, ",")
	seedDigest := sha256.Sum256([]byte(machineSeed))
	seedHex := hex.EncodeToString(seedDigest[:])

	info := struct {
		System struct {
			Manufacturer string `json:"manufacturer"`
			Model        string `json:"model"`
			Serial       string `json:"serial"`
			UUID         string `json:"uuid"`
		} `json:"system"`
		CPU struct {
			Manufacturer  string `json:"manufacturer"`
			Brand         string `json:"brand"`
			Cores         int    `json:"cores"`
			PhysicalCores int    `json:"physicalCores"`
		} `json:"cpu"`
		OS struct {
			Platform string `json:"platform"`
			Distro   string `json:"distro"`
			Arch     string `json:"arch"`
			Hostname string `json:"hostname"`
		} `json:"os"`
		Runtime struct {
			NodeVersion string `json:"nodeVersion"`
			Platform    string `json:"platform"`
			Arch        string `json:"arch"`
			Shell       string `json:"shell"`
			CPUCount    int    `json:"cpuCount"`
		} `json:"runtime"`
		Network struct {
			MACAddresses   []string `json:"macAddresses"`
			InterfaceCount int      `json:"interfaceCount"`
		} `json:"network"`
		MachineID          string `json:"machineId"`
		FingerprintVersion string `json:"fingerprintVersion"`
	}{}
	info.System.Serial = seedHex[:16] // deterministic per-host persona serial
	// UUID-shaped 8-4-4-4-12 slice of the same digest.
	info.System.UUID = seedHex[16:24] + "-" + seedHex[24:28] + "-" +
		seedHex[28:32] + "-" + seedHex[32:36] + "-" + seedHex[36:52]
	info.CPU.Manufacturer = "GenuineIntel"
	info.CPU.Brand = "Generic CPU"
	info.CPU.Cores = cpuCount
	info.CPU.PhysicalCores = cpuCount // no portable socket topology; cores are the deterministic choice
	info.OS.Platform = nodePlatform(runtime.GOOS)
	switch runtime.GOOS {
	case "darwin":
		info.OS.Distro = "Apple macOS"
	case "linux":
		info.OS.Distro = "Ubuntu Linux"
	default:
		info.OS.Distro = "Microsoft Windows 11 Pro"
	}
	info.OS.Arch = nodeArch(runtime.GOARCH)
	info.OS.Hostname = hostname
	info.Runtime.NodeVersion = "v22.14.0" // pinned persona: the CLI ships its own Node
	info.Runtime.Platform = info.OS.Platform
	info.Runtime.Arch = info.OS.Arch
	info.Runtime.Shell = "/bin/bash" // pinned persona: Node detectShell equivalent
	info.Runtime.CPUCount = cpuCount
	info.Network.MACAddresses = macs
	info.Network.InterfaceCount = ifaceCount
	info.MachineID = seedHex[:32] // node-machine-id shape: 32 hex chars
	info.FingerprintVersion = "2.0"

	sum := sha256.Sum256([]byte(mustJSON(info)))
	return "enhanced-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// mustJSON marshals v; the input is plain data built above, so an error is
// impossible in practice — panic loudly rather than send a degraded
// fingerprint.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("login: marshal fingerprint info: %v", err))
	}
	return b
}
