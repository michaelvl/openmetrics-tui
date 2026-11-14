package managed

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mode is how the dashboard reaches its data.
type Mode int

const (
	// ModeExternal talks to an existing Prometheus-compatible API.
	ModeExternal Mode = iota
	// ModeManaged starts kubectl proxy and a local VictoriaMetrics.
	ModeManaged
)

// SelectMode applies the backend precedence: --prom, --managed, else the
// dashboard's datasource.url. For ModeExternal it returns the URL to use.
func SelectMode(promURL string, managed bool, datasourceURL string) (Mode, string, error) {
	switch {
	case promURL != "" && managed:
		return 0, "", errors.New("--prom and --managed are mutually exclusive")
	case promURL != "":
		return ModeExternal, promURL, nil
	case managed:
		return ModeManaged, "", nil
	case datasourceURL != "":
		return ModeExternal, datasourceURL, nil
	}
	return 0, "", errors.New("no backend: pass --prom URL to use an existing Prometheus or VictoriaMetrics, " +
		"pass --managed to start kubectl proxy and a local VictoriaMetrics, or set datasource.url in the dashboard")
}

var retentionRe = regexp.MustCompile(`^([0-9]+)([smhdwMy])$`)

var retentionUnit = map[string]time.Duration{
	"s": time.Second, "m": time.Minute, "h": time.Hour,
	"d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
	"M": 30 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
}

// minRetention is the smallest -retentionPeriod VictoriaMetrics accepts
// (verified against 1.156.0, see docs/managed-verification.md).
const minRetention = 24 * time.Hour

// ValidateRetention checks a --retention value. A unit suffix is required
// because VictoriaMetrics reads a bare number as months.
func ValidateRetention(s string) error {
	m := retentionRe.FindStringSubmatch(s)
	if m == nil {
		return errors.New("retention " + strconv.Quote(s) + " needs a number and a unit suffix (s, m, h, d, w, M, y), for example 24h or 7d")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return err
	}
	if time.Duration(n)*retentionUnit[m[2]] < minRetention {
		return errors.New("retention " + strconv.Quote(s) + " is below the 24h minimum VictoriaMetrics accepts")
	}
	return nil
}

var versionRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// MinVersion is the oldest victoria-metrics we expect to work. It is a
// conservative floor, not a tested bound: only 1.156.0 was exercised.
const MinVersion = "1.100.0"

// CheckVersion compares `victoria-metrics -version` output with MinVersion.
// It returns a warning, or "" when the version is fine.
func CheckVersion(output string) string {
	cur := versionRe.FindStringSubmatch(output)
	if cur == nil {
		return "cannot parse victoria-metrics version " + strconv.Quote(strings.TrimSpace(output)) + "; expected at least " + MinVersion
	}
	lo := versionRe.FindStringSubmatch(MinVersion)
	for i := 1; i <= 3; i++ {
		c, _ := strconv.Atoi(cur[i])
		m, _ := strconv.Atoi(lo[i])
		if c != m {
			if c < m {
				return "victoria-metrics " + cur[0] + " is older than the minimum " + MinVersion + "; managed mode may not work"
			}
			return ""
		}
	}
	return ""
}
