package radio

import (
	"fmt"
	"sort"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/sysfs"
)

// Role is what a radio does.
type Role string

// Roles.
const (
	RoleMesh   Role = "mesh"
	RoleAP     Role = "ap"
	RoleMeshAP Role = "mesh+ap"
	// The second mesh link (profile radio2, interface mesh1; decision D-09).
	RoleMesh2   Role = "mesh2"
	RoleMesh2AP Role = "mesh2+ap"
	RoleUnused  Role = "unused"
)

// Notice is a warning or error produced while planning.
type Notice struct {
	Level   string `json:"level"` // info, warning, error
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Input is everything the planner needs.
type Input struct {
	Radios    []*Radio
	MeshBand  string // the mesh profile's band
	Mesh2Band string // the profile's second mesh link band (radio2), "" for none
	APEnabled bool
	Roles     config.RoleOverrides
	// Current is the active assignment (fingerprint → role), or nil at boot.
	// With Current set, roles only change when a radio is lost: a newly
	// present radio only fills an empty role.
	Current map[string]Role
}

// Plan is the result: which radio carries which role.
type Plan struct {
	Mesh        *Radio          `json:"-"`
	Mesh2       *Radio          `json:"-"` // second mesh link (mesh1), or nil
	AP          *Radio          `json:"-"`
	Roles       map[string]Role `json:"roles"` // fingerprint → role, for every radio present
	APBand      string          `json:"ap_band"`
	Notices     []Notice        `json:"notices"`
	Recommended map[string]Role `json:"recommended"` // the boot-time result for the radios present
	Differs     bool            `json:"recommendation_differs"`
}

func (p *Plan) note(level, code, format string, args ...any) {
	p.Notices = append(p.Notices, Notice{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
}

// MeshFingerprint is the mesh radio's fingerprint or "".
func (p *Plan) MeshFingerprint() string {
	if p.Mesh == nil {
		return ""
	}
	return p.Mesh.Fingerprint
}

// Mesh2Fingerprint is the second mesh link's radio fingerprint or "".
func (p *Plan) Mesh2Fingerprint() string {
	if p.Mesh2 == nil {
		return ""
	}
	return p.Mesh2.Fingerprint
}

// APSharesMesh2 reports whether the AP runs on the second mesh link's radio.
func (p *Plan) APSharesMesh2() bool { return p.AP != nil && p.AP == p.Mesh2 }

// APFingerprint is the AP radio's fingerprint or "".
func (p *Plan) APFingerprint() string {
	if p.AP == nil {
		return ""
	}
	return p.AP.Fingerprint
}

// APBandFor picks the AP band: 5 GHz unless the mesh is there, then 2.4 GHz
// (architecture §10: always a different band from a standard Wi-Fi mesh).
func APBandFor(meshBand string) string {
	if meshBand == config.Band5G {
		return config.Band2G
	}
	return config.Band5G
}

func busRank(d sysfs.Device) int {
	switch d.Bus {
	case sysfs.BusPCI:
		return 3
	case sysfs.BusUSB:
		if d.USBSpeedMbps >= 5000 {
			return 2
		}
		return 1
	}
	return 0
}

func (in Input) meshEligible(r *Radio) (bool, string) {
	return in.meshEligibleFor(r, in.MeshBand, false)
}

func (in Input) meshEligibleAs(r *Radio, explicit bool) (bool, string) {
	return in.meshEligibleFor(r, in.MeshBand, explicit)
}

// meshEligibleAs checks a radio for the mesh role. explicit is true when the
// node config pins the role to this radio, which overrides InUse. InUse is
// checked last so that a radio which could never carry the mesh is reported
// for that reason, not with advice to pin it.
func (in Input) meshEligibleFor(r *Radio, band string, explicit bool) (bool, string) {
	switch {
	case r.Ignored:
		return false, "ignored by config"
	case r.Broken != "":
		return false, r.Broken
	case r.HardBlocked:
		return false, "blocked by a hardware rf-kill switch"
	case !r.Caps.MeshPoint:
		return false, "no mesh point support"
	case !r.HasBand(band):
		return false, "no " + band + " band"
	case r.InUse != "" && !explicit:
		return false, "in use by the system (" + r.InUse + "); pin the role to it in node.yaml to take it over"
	}
	return true, ""
}

func (in Input) apEligible(r *Radio, apBand string) (bool, string) {
	return in.apEligibleAs(r, apBand, false)
}

func (in Input) apEligibleAs(r *Radio, apBand string, explicit bool) (bool, string) {
	switch {
	case r.Ignored:
		return false, "ignored by config"
	case r.HardBlocked:
		return false, "blocked by a hardware rf-kill switch"
	case !r.Caps.AP:
		return false, "no AP support"
	case !r.HasBand(apBand) && !r.HasBand(config.Band2G) && !r.HasBand(config.Band5G):
		return false, "no 2.4 or 5 GHz band"
	case r.InUse != "" && !explicit:
		return false, "in use by the system (" + r.InUse + "); pin the role to it in node.yaml to take it over"
	}
	return true, ""
}

func power(r *Radio, band string) float64 {
	if p, ok := r.Power[band]; ok {
		return p.DBm
	}
	return -1
}

// meshTrust ranks how well mesh point is known to work on a radio. Driver
// power figures can't break this tie: some drivers report the regulatory
// limit, so an untested adapter would otherwise beat a proven one.
func meshTrust(r *Radio) int {
	if r.HWDB == nil {
		return 1
	}
	switch r.HWDB.MeshPoint {
	case hwdb.Verified:
		return 3
	case hwdb.Reported:
		return 2
	}
	return 1
}

// rankMesh orders mesh candidates best first (architecture §5.2 table), after
// first preferring radios whose mesh support is better established.
func rankMesh(cands []*Radio, band string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ta, tb := meshTrust(a), meshTrust(b); ta != tb {
			return ta > tb
		}
		if pa, pb := power(a, band), power(b, band); pa != pb {
			return pa > pb
		}
		if a.Caps.Streams != b.Caps.Streams {
			return a.Caps.Streams > b.Caps.Streams
		}
		if ra, rb := busRank(a.Device), busRank(b.Device); ra != rb {
			return ra > rb
		}
		return a.Fingerprint < b.Fingerprint
	})
}

// rankAP orders AP candidates best first: lowest power, then supports the
// preferred AP band, then fingerprint.
func rankAP(cands []*Radio, apBand string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ha, hb := a.HasBand(apBand), b.HasBand(apBand); ha != hb {
			return ha
		}
		if pa, pb := power(a, apBand), power(b, apBand); pa != pb {
			return pa < pb
		}
		if ha, hb := a.HasBand(config.Band5G), b.HasBand(config.Band5G); ha != hb {
			return ha
		}
		return a.Fingerprint < b.Fingerprint
	})
}

// MakePlan assigns roles.
func MakePlan(in Input) *Plan {
	p := plan(in)
	if in.Current != nil {
		fresh := in
		fresh.Current = nil
		rec := plan(fresh)
		p.Recommended = rec.Roles
		for fp, role := range rec.Roles {
			if p.Roles[fp] != role {
				p.Differs = true
			}
		}
	} else {
		p.Recommended = p.Roles
	}
	return p
}

func plan(in Input) *Plan {
	p := &Plan{Roles: map[string]Role{}, APBand: APBandFor(in.MeshBand)}
	for _, r := range in.Radios {
		p.Roles[r.Fingerprint] = RoleUnused
	}
	if len(in.Radios) == 0 {
		if in.Roles.Mesh.Mode == config.RoleDisabled {
			p.note("info", "no-radios", "no Wi-Fi radios found (mesh role disabled; wired ports only)")
		} else {
			p.note("error", "no-radios", "no Wi-Fi radios found")
		}
		return p
	}

	// Mesh ------------------------------------------------------------------
	var meshCands []*Radio
	for _, r := range in.Radios {
		if ok, _ := in.meshEligible(r); ok {
			meshCands = append(meshCands, r)
		}
	}
	rankMesh(meshCands, in.MeshBand)

	switch in.Roles.Mesh.Mode {
	case config.RoleDisabled:
		p.note("info", "mesh-disabled", "mesh role disabled in node config")
	case "radio", "port":
		var pinned *Radio
		if in.Roles.Mesh.Mode == "radio" {
			pinned = Find(in.Radios, in.Roles.Mesh.Radio)
		} else {
			pinned = FindByPort(in.Radios, in.Roles.Mesh.Port)
		}
		switch {
		case pinned == nil:
			p.note("error", "mesh-pin-missing", "mesh is pinned to %s%s, which is not present; no mesh radio", in.Roles.Mesh.Mode+" ", in.Roles.Mesh.Radio+in.Roles.Mesh.Port)
		default:
			if ok, why := in.meshEligibleAs(pinned, true); !ok {
				p.note("error", "mesh-pin-ineligible", "mesh is pinned to %s, which cannot carry it: %s", pinned.Describe(), why)
			} else {
				p.Mesh = pinned
			}
		}
	default: // auto
		if cur := currentHolder(in, RoleMesh, RoleMeshAP); cur != nil {
			if ok, _ := in.meshEligible(cur); ok {
				p.Mesh = cur
			}
		}
		if p.Mesh == nil && len(meshCands) > 0 {
			p.Mesh = meshCands[0]
		}
		if p.Mesh == nil {
			if len(in.Radios) > 0 {
				p.note("error", "no-mesh-radio", "no radio can carry the %s mesh: %s. Plug in an adapter that supports mesh point (docs/hardware.md lists tested ones)", in.MeshBand, ineligibleSummary(in))
			}
		}
	}

	// Second mesh link ----------------------------------------------------------
	if in.Mesh2Band != "" {
		p.planMesh2(in)
	}

	// AP ----------------------------------------------------------------------
	if !in.APEnabled {
		p.note("info", "ap-disabled", "client AP disabled in node config")
	} else {
		var apCands []*Radio
		for _, r := range in.Radios {
			if r == p.Mesh || r == p.Mesh2 {
				continue
			}
			if ok, _ := in.apEligible(r, p.APBand); ok {
				apCands = append(apCands, r)
			}
		}
		rankAP(apCands, p.APBand)

		switch in.Roles.AP.Mode {
		case config.RoleDisabled:
			p.note("info", "ap-disabled", "AP role disabled in node config")
		case "radio", "port":
			var pinned *Radio
			if in.Roles.AP.Mode == "radio" {
				pinned = Find(in.Radios, in.Roles.AP.Radio)
			} else {
				pinned = FindByPort(in.Radios, in.Roles.AP.Port)
			}
			switch {
			case pinned == nil:
				p.note("error", "ap-pin-missing", "AP is pinned to a radio that is not present; no AP")
			case pinned == p.Mesh || pinned == p.Mesh2:
				if pinned.Caps.MeshAndAP {
					p.AP = pinned
				} else {
					p.note("error", "ap-pin-conflict", "AP is pinned to the mesh radio %s, which cannot run both at once", pinned.Describe())
				}
			default:
				if ok, why := in.apEligibleAs(pinned, p.APBand, true); !ok {
					p.note("error", "ap-pin-ineligible", "AP is pinned to %s, which cannot carry it: %s", pinned.Describe(), why)
				} else {
					p.AP = pinned
				}
			}
		default: // auto
			if cur := currentHolder(in, RoleAP); cur != nil && cur != p.Mesh && cur != p.Mesh2 {
				if ok, _ := in.apEligible(cur, p.APBand); ok {
					p.AP = cur
				}
			}
			if p.AP == nil && len(apCands) > 0 {
				p.AP = apCands[0]
			}
			if p.AP == nil && p.Mesh != nil {
				// Share a mesh radio: the first link's, else the second's.
				switch {
				case p.Mesh.Caps.MeshAndAP:
					p.AP = p.Mesh
					p.note("warning", "ap-shares-mesh-radio", "no free radio: the client AP shares the mesh radio, channel and airtime")
				case p.Mesh2 != nil && p.Mesh2.Caps.MeshAndAP:
					p.AP = p.Mesh2
					p.note("warning", "ap-shares-mesh-radio", "no free radio: the client AP shares the second mesh link's radio, channel and airtime")
				default:
					p.note("warning", "no-ap-radio", "no radio left for the client AP: connect the EUD by cable")
				}
			}
		}
		if p.AP != nil && p.AP != p.Mesh && p.AP != p.Mesh2 && !p.AP.HasBand(p.APBand) {
			other := config.Band2G
			if p.APBand == config.Band2G {
				other = config.Band5G
			}
			p.note("warning", "ap-band-fallback", "AP radio %s has no %s band; the AP will use %s, the same band as the mesh", p.AP.Describe(), p.APBand, other)
		}
	}

	for _, r := range in.Radios {
		if r.InUse != "" && r != p.Mesh && r != p.Mesh2 && r != p.AP {
			p.note("info", "radio-in-use", "%s left alone: %s", r.Describe(), r.InUse)
		}
	}

	// Roles map ------------------------------------------------------------------
	if p.Mesh != nil {
		p.Roles[p.Mesh.Fingerprint] = RoleMesh
	}
	if p.Mesh2 != nil {
		p.Roles[p.Mesh2.Fingerprint] = RoleMesh2
	}
	if p.AP != nil {
		switch p.AP {
		case p.Mesh:
			p.Roles[p.AP.Fingerprint] = RoleMeshAP
		case p.Mesh2:
			p.Roles[p.AP.Fingerprint] = RoleMesh2AP
		default:
			p.Roles[p.AP.Fingerprint] = RoleAP
		}
	}
	unused := 0
	for _, r := range in.Radios {
		if p.Roles[r.Fingerprint] == RoleUnused && !r.Ignored {
			unused++
		}
	}
	if unused > 0 && p.Mesh != nil && (p.AP != nil || !in.APEnabled) {
		p.note("info", "radios-unused", "%d radio(s) unused: no role left for them", unused)
	}
	return p
}

// planMesh2 picks the radio for the second mesh link (profile radio2): one
// that supports its band and is not the first link's radio. The links come
// before the AP, which can share a mesh radio; a missing second link only
// leaves the node on one.
func (p *Plan) planMesh2(in Input) {
	band := in.Mesh2Band
	var cands []*Radio
	for _, r := range in.Radios {
		if r == p.Mesh {
			continue
		}
		if ok, _ := in.meshEligibleFor(r, band, false); ok {
			cands = append(cands, r)
		}
	}
	rankMesh(cands, band)
	sel := in.Roles.Mesh2
	switch sel.Mode {
	case config.RoleDisabled:
		p.note("info", "mesh2-disabled", "second mesh link (%s) disabled in node config", band)
	case "radio", "port":
		var pinned *Radio
		if sel.Mode == "radio" {
			pinned = Find(in.Radios, sel.Radio)
		} else {
			pinned = FindByPort(in.Radios, sel.Port)
		}
		switch {
		case pinned == nil:
			p.note("error", "mesh2-pin-missing", "the second mesh link is pinned to %s%s, which is not present", sel.Mode+" ", sel.Radio+sel.Port)
		case pinned == p.Mesh:
			p.note("error", "mesh2-pin-conflict", "the second mesh link is pinned to the first link's radio %s", pinned.Describe())
		default:
			if ok, why := in.meshEligibleFor(pinned, band, true); !ok {
				p.note("error", "mesh2-pin-ineligible", "the second mesh link is pinned to %s, which cannot carry it: %s", pinned.Describe(), why)
			} else {
				p.Mesh2 = pinned
			}
		}
	default: // auto
		if cur := currentHolder(in, RoleMesh2, RoleMesh2AP); cur != nil && cur != p.Mesh {
			if ok, _ := in.meshEligibleFor(cur, band, false); ok {
				p.Mesh2 = cur
			}
		}
		if p.Mesh2 == nil && len(cands) > 0 {
			p.Mesh2 = cands[0]
		}
		if p.Mesh2 == nil {
			p.note("warning", "no-mesh2-radio", "no second radio can carry the %s mesh link (radio2): this node meshes on one link only", band)
		}
	}
}

func currentHolder(in Input, roles ...Role) *Radio {
	for fp, role := range in.Current {
		for _, want := range roles {
			if role == want {
				if r := Find(in.Radios, fp); r != nil {
					return r
				}
			}
		}
	}
	return nil
}

func ineligibleSummary(in Input) string {
	s := ""
	for _, r := range in.Radios {
		if ok, why := in.meshEligible(r); !ok {
			if s != "" {
				s += "; "
			}
			s += r.Phy + ": " + why
		}
	}
	return s
}
