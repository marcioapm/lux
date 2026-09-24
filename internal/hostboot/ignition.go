package hostboot

import (
	"encoding/base64"
	"encoding/json"
)

// ignitionSpec is the Ignition config spec version this package emits.
// 3.4.0 is what Fedora CoreOS's stable stream has read since FCOS
// switched to Ignition 2.14 (2023); it is also the newest spec every
// currently supported stable release understands, and it has the
// `arn:` S3 access point source we do not need but the plain `data:`
// scheme we do (present since spec 2.0.0).
const ignitionSpec = "3.4.0"

type ignitionConfig struct {
	Ignition ignitionMeta    `json:"ignition"`
	Storage  ignitionStorage `json:"storage,omitempty"`
	Systemd  ignitionSystemd `json:"systemd,omitempty"`
}

type ignitionMeta struct {
	Version string `json:"version"`
}

type ignitionStorage struct {
	Files []ignitionFile `json:"files,omitempty"`
}

type ignitionFile struct {
	Path string `json:"path"`
	// Mode: Ignition wants decimal (0644 -> 420), not octal-as-written.
	Mode     int                    `json:"mode,omitempty"`
	Contents *ignitionFileContents  `json:"contents,omitempty"`
	Append   []ignitionFileContents `json:"append,omitempty"`
}

type ignitionFileContents struct {
	Source string `json:"source"`
}

type ignitionSystemd struct {
	Units []ignitionUnit `json:"units,omitempty"`
}

type ignitionUnit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Mask     bool   `json:"mask,omitempty"`
	Contents string `json:"contents,omitempty"`
}

func dataURL(content string) string {
	return "data:;base64," + base64.StdEncoding.EncodeToString([]byte(content))
}

func boolPtr(b bool) *bool { return &b }

// Ignition renders the Ignition v3.4.0 config Fedora CoreOS reads from EC2
// user data (spec 3.4.0: see ignitionSpec). It writes /etc/lux/runner.env,
// installs the fetch-binaries script and the lux-runner unit (enabled),
// and masks zincati.service: runners are disposable and must never
// auto-update and reboot mid-Run. The runner's subuid/subgid range is
// appended by the fetch script itself (FetchBinariesScript), not here:
// Ignition's `append` always appends, with no "only if absent" the script
// format also needs, so both formats share the one idempotent append
// instead of each risking a different answer to "what if it's already
// there".
func Ignition(env Env) ([]byte, error) {
	cfg := ignitionConfig{
		Ignition: ignitionMeta{Version: ignitionSpec},
		Storage: ignitionStorage{
			Files: []ignitionFile{
				{Path: "/etc/lux/runner.env", Mode: 0o600, Contents: &ignitionFileContents{Source: dataURL(env.Lines())}},
				{Path: FetchBinariesPath, Mode: 0o755, Contents: &ignitionFileContents{Source: dataURL(FetchBinariesScript)}},
			},
		},
		Systemd: ignitionSystemd{
			Units: []ignitionUnit{
				{Name: UnitName, Enabled: boolPtr(true), Contents: Unit},
				{Name: "zincati.service", Mask: true},
			},
		},
	}
	return json.Marshal(cfg)
}
