package batadv

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ModuleVersionPath is where the kernel exposes the loaded module's version.
const ModuleVersionPath = "/sys/module/batman_adv/version"

// Version is a batman-adv release such as 2026.3.
type Version struct {
	Year  int
	Minor int
}

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Year, v.Minor) }

// IsZero reports whether the version is unset.
func (v Version) IsZero() bool { return v.Year == 0 }

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Year != o.Year {
		return v.Year < o.Year
	}
	return v.Minor < o.Minor
}

var reRelease = regexp.MustCompile(`^(\d{4})\.(\d+)`)

// ParseVersion parses a release string like "2026.3" or "2024.1-openwrt".
func ParseVersion(s string) (Version, error) {
	m := reRelease.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("not a batman-adv release version: %q", s)
	}
	y, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	return Version{Year: y, Minor: n}, nil
}

// ModuleInfo describes the loaded batman_adv module.
type ModuleInfo struct {
	Loaded  bool
	Raw     string  // exact contents of /sys/module/batman_adv/version
	Release Version // parsed release, zero if Raw is not a release string
	// InTreeKernelVersion is set when the module reports the kernel's own
	// version instead of a release (recent in-tree builds do this). Features
	// must then be inferred from the kernel version.
	InTreeKernelVersion string
	// Path is the module file, when it can be found. A path under "updates/"
	// means the DKMS build is in use rather than the kernel's own copy.
	Path string
}

// ReadModuleInfo reads the loaded module's version from sysfs.
func ReadModuleInfo() ModuleInfo {
	return readModuleInfo(ModuleVersionPath)
}

func readModuleInfo(path string) ModuleInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return ModuleInfo{}
	}
	mi := ModuleInfo{Loaded: true, Raw: strings.TrimSpace(string(b))}
	if v, err := ParseVersion(mi.Raw); err == nil {
		mi.Release = v
	} else {
		mi.InTreeKernelVersion = mi.Raw
	}
	return mi
}

// MinInTreeKernelForMulticastPacketType is the first mainline kernel whose
// in-tree batman-adv includes the 2024.0 multicast packet type. Used only
// when the module reports a kernel version rather than a release.
const MinInTreeKernelForMulticastPacketType = "6.9"

// EffectiveRelease returns the release to compare against the version gate:
// the parsed release when present, otherwise the oldest release the kernel
// version implies (2024.0 for 6.9+, 2022.3 for older), with inferred=true.
func (mi ModuleInfo) EffectiveRelease() (v Version, inferred bool) {
	if !mi.Release.IsZero() {
		return mi.Release, false
	}
	if mi.InTreeKernelVersion == "" {
		return Version{}, false
	}
	if kernelAtLeast(mi.InTreeKernelVersion, MinInTreeKernelForMulticastPacketType) {
		return Version{Year: 2024, Minor: 0}, true
	}
	return Version{Year: 2022, Minor: 3}, true
}

var reKernel = regexp.MustCompile(`^(\d+)\.(\d+)`)

func kernelAtLeast(have, want string) bool {
	h := reKernel.FindStringSubmatch(have)
	w := reKernel.FindStringSubmatch(want)
	if h == nil || w == nil {
		return false
	}
	hm, _ := strconv.Atoi(h[1])
	hn, _ := strconv.Atoi(h[2])
	wm, _ := strconv.Atoi(w[1])
	wn, _ := strconv.Atoi(w[2])
	if hm != wm {
		return hm > wm
	}
	return hn >= wn
}
