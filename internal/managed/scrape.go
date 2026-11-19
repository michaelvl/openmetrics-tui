package managed

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/michaelvl/openmetrics-tui/internal/dashboard"
)

// Default annotation names, used when scrape.annotation is set without
// scrape.port or scrape.path.
const (
	defaultPortAnnotation = "prometheus.io/port"
	defaultPathAnnotation = "prometheus.io/path"
	defaultPath           = "/metrics"
)

// ScrapeParams is what the generator needs beyond the dashboard's scrape
// section.
type ScrapeParams struct {
	// Scrape is the dashboard's scrape section; nil scrapes every Running
	// pod that declares a container port.
	Scrape *dashboard.Scrape
	// Namespaces overrides Scrape.Namespaces (--namespace). Empty with no
	// Scrape means all namespaces.
	Namespaces []string
	// ProxyAddr is the kubectl proxy address, host:port. Ignored with InCluster.
	ProxyAddr string
	// InCluster scrapes pods directly at their pod IP, with service discovery
	// using the pod's ServiceAccount, instead of through kubectl proxy.
	InCluster bool
}

type scrapeFile struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

type scrapeJob struct {
	JobName        string        `yaml:"job_name"`
	ScrapeInterval string        `yaml:"scrape_interval"`
	Scheme         string        `yaml:"scheme"`
	KubernetesSD   []kubernetes  `yaml:"kubernetes_sd_configs"`
	Relabel        []relabelRule `yaml:"relabel_configs"`
}

type kubernetes struct {
	Role       string       `yaml:"role"`
	APIServer  string       `yaml:"api_server,omitempty"`
	Namespaces *sdNamespace `yaml:"namespaces,omitempty"`
	Selectors  []sdSelector `yaml:"selectors,omitempty"`
}

type sdNamespace struct {
	Names []string `yaml:"names,flow"`
}

type sdSelector struct {
	Role  string `yaml:"role"`
	Label string `yaml:"label"`
}

type relabelRule struct {
	SourceLabels []string `yaml:"source_labels,omitempty,flow"`
	Action       string   `yaml:"action,omitempty"`
	// Regex is a pointer so that an explicit empty regex is kept.
	Regex       *string `yaml:"regex,omitempty"`
	Replacement string  `yaml:"replacement,omitempty"`
	TargetLabel string  `yaml:"target_label,omitempty"`
}

func rx(s string) *string { return &s }

// metaAnnotation converts an annotation name to the relabel meta label that
// carries its value: every character outside [A-Za-z0-9_] becomes "_".
func metaAnnotation(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "__meta_kubernetes_pod_annotation_" + b.String()
}

const (
	lblPhase = "__meta_kubernetes_pod_phase"
	lblNS    = "__meta_kubernetes_namespace"
	lblPod   = "__meta_kubernetes_pod_name"
	lblPort  = "__meta_kubernetes_pod_container_port_number"
	lblIP    = "__meta_kubernetes_pod_ip"
	lblTmp   = "__tmp_path"
	proxyFmt = "/api/v1/namespaces/$1/pods/$2:$3/proxy"
)

// GenerateScrapeConfig renders the VictoriaMetrics scrape config that scrapes
// pods through kubectl proxy (see the plan, Appendix B). Every target is
// reached at the proxy address with the pod-proxy path as its metrics path.
// With InCluster each target is reached at its pod IP and port with the plain
// metrics path, and discovery uses the in-pod ServiceAccount (no api_server).
// Everything else, including the namespace, pod and instance labels, is shared.
func GenerateScrapeConfig(p ScrapeParams) ([]byte, error) {
	if p.ProxyAddr == "" && !p.InCluster {
		return nil, fmt.Errorf("proxy address is empty")
	}
	s := p.Scrape
	interval := 15 * time.Second
	if s != nil && s.Interval > 0 {
		interval = s.Interval
	}
	namespaces := p.Namespaces
	if len(namespaces) == 0 && s != nil {
		namespaces = s.Namespaces
	}
	sd := kubernetes{Role: "pod"}
	if !p.InCluster {
		sd.APIServer = "http://" + p.ProxyAddr
	}
	if len(namespaces) > 0 {
		sd.Namespaces = &sdNamespace{Names: namespaces}
	}

	rules := []relabelRule{{SourceLabels: []string{lblPhase}, Action: "keep", Regex: rx("Running")}}
	switch {
	case s != nil && s.Annotation != "":
		portAnn, pathAnn := s.Port, s.Path
		if portAnn == "" {
			portAnn = defaultPortAnnotation
		}
		if pathAnn == "" {
			pathAnn = defaultPathAnnotation
		}
		portLbl := metaAnnotation(portAnn)
		rules = append(rules,
			relabelRule{SourceLabels: []string{metaAnnotation(s.Annotation)}, Action: "keep", Regex: rx("true")},
			// A pod without the port annotation is skipped; without this rule it
			// would keep the default metrics path and scrape the API server itself.
			relabelRule{SourceLabels: []string{portLbl}, Action: "keep", Regex: rx(".+")},
			relabelRule{SourceLabels: []string{metaAnnotation(pathAnn)}, Regex: rx("(.+)"), TargetLabel: lblTmp},
			relabelRule{SourceLabels: []string{lblTmp}, Regex: rx(""), Replacement: defaultPath, TargetLabel: lblTmp},
		)
		if p.InCluster {
			rules = append(rules,
				relabelRule{SourceLabels: []string{lblTmp}, TargetLabel: "__metrics_path__"},
				relabelRule{SourceLabels: []string{lblIP, portLbl}, Regex: rx("(.+);(.+)"), Replacement: "$1:$2", TargetLabel: "__address__"},
			)
		} else {
			rules = append(rules,
				relabelRule{
					SourceLabels: []string{lblNS, lblPod, portLbl, lblTmp},
					Regex:        rx("(.+);(.+);(.+);(.+)"),
					Replacement:  "/api/v1/namespaces/$1/pods/$2:$3/proxy$4",
					TargetLabel:  "__metrics_path__",
				},
				relabelRule{TargetLabel: "__address__", Replacement: p.ProxyAddr},
			)
		}
	default:
		if s != nil && s.Selector != "" {
			sd.Selectors = []sdSelector{{Role: "pod", Label: s.Selector}}
		}
		path := defaultPath
		if s != nil && s.Path != "" {
			path = s.Path
		}
		path = strings.ReplaceAll(path, "$", "$$")
		port := ""
		if s != nil {
			port = strings.ReplaceAll(s.Port, "$", "$$")
		}
		switch {
		case p.InCluster && port != "":
			rules = append(rules,
				relabelRule{TargetLabel: "__metrics_path__", Replacement: path},
				relabelRule{SourceLabels: []string{lblIP}, Regex: rx("(.+)"), Replacement: "$1:" + port, TargetLabel: "__address__"},
			)
		case p.InCluster:
			rules = append(rules,
				relabelRule{SourceLabels: []string{lblPort}, Action: "keep", Regex: rx(".+")},
				relabelRule{TargetLabel: "__metrics_path__", Replacement: path},
				relabelRule{SourceLabels: []string{lblIP, lblPort}, Regex: rx("(.+);(.+)"), Replacement: "$1:$2", TargetLabel: "__address__"},
			)
		case port != "":
			// One fixed port: containers that each declare it give identical
			// targets, which VictoriaMetrics collapses into one.
			rules = append(rules, relabelRule{
				SourceLabels: []string{lblNS, lblPod},
				Regex:        rx("(.+);(.+)"),
				Replacement:  "/api/v1/namespaces/$1/pods/$2:" + port + "/proxy" + path,
				TargetLabel:  "__metrics_path__",
			}, relabelRule{TargetLabel: "__address__", Replacement: p.ProxyAddr})
		default:
			// Every declared container port is a target; pods without one are skipped.
			rules = append(rules,
				relabelRule{SourceLabels: []string{lblPort}, Action: "keep", Regex: rx(".+")},
				relabelRule{
					SourceLabels: []string{lblNS, lblPod, lblPort},
					Regex:        rx("(.+);(.+);(.+)"),
					Replacement:  proxyFmt + path,
					TargetLabel:  "__metrics_path__",
				},
				relabelRule{TargetLabel: "__address__", Replacement: p.ProxyAddr})
		}
	}
	rules = append(rules,
		relabelRule{SourceLabels: []string{lblNS}, TargetLabel: "namespace"},
		relabelRule{SourceLabels: []string{lblPod}, TargetLabel: "pod"},
	)
	if s == nil || s.Annotation == "" && s.Port == "" {
		rules = append(rules, relabelRule{
			SourceLabels: []string{lblPod, lblPort}, Regex: rx("(.+);(.+)"),
			Replacement: "$1:$2", TargetLabel: "instance",
		})
	} else {
		rules = append(rules, relabelRule{SourceLabels: []string{lblPod}, TargetLabel: "instance"})
	}
	if s != nil && s.Annotation != "" {
		rules = append(rules, relabelRule{Regex: rx(lblTmp), Action: "labeldrop"})
	}

	job := "pods-via-proxy"
	if p.InCluster {
		job = "pods"
	}
	out := scrapeFile{ScrapeConfigs: []scrapeJob{{
		JobName:        job,
		ScrapeInterval: fmt.Sprintf("%ds", int(interval.Seconds())),
		Scheme:         "http",
		KubernetesSD:   []kubernetes{sd},
		Relabel:        rules,
	}}}
	return yaml.Marshal(out)
}
